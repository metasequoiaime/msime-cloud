package account

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

// apngTestFrame 是测试 APNG 的一帧：像素和它在画布上的位置。
type apngTestFrame struct {
	img  image.Image
	x, y uint32
}

type testChunk struct {
	kind string
	body []byte
}

// apngTestImage 生成一张每帧不同的半透明 NRGBA，半透明保证 image/png 每帧都编码成同一种颜色类型（RGBA 8 位），各帧可以共用一个 IHDR。
func apngTestImage(width, height, seed int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = byte(i*13 + seed*41)
	}
	img.SetNRGBA(0, 0, color.NRGBA{R: byte(seed), G: 1, B: 2, A: 128})
	return img
}

func testPNGChunks(t *testing.T, data []byte) []testChunk {
	t.Helper()
	if !bytes.HasPrefix(data, pngSignature) {
		t.Fatal("not a PNG")
	}
	var chunks []testChunk
	for pos := len(pngSignature); pos < len(data); {
		length := int(binary.BigEndian.Uint32(data[pos:]))
		chunks = append(chunks, testChunk{string(data[pos+4 : pos+8]), data[pos+8 : pos+8+length]})
		pos += 12 + length
	}
	return chunks
}

func testPNG(chunks []testChunk) []byte {
	out := append([]byte{}, pngSignature...)
	for _, c := range chunks {
		out = appendPNGChunk(out, c.kind, c.body)
	}
	return out
}

// encodeTestFrame 用 image/png 编码一帧，返回 IHDR 载荷、PLTE 与 tRNS 块和全部 IDAT 载荷。
func encodeTestFrame(t *testing.T, img image.Image) ([]byte, []testChunk, [][]byte) {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	var ihdr []byte
	var palette []testChunk
	var idat [][]byte
	for _, c := range testPNGChunks(t, out.Bytes()) {
		switch c.kind {
		case "IHDR":
			ihdr = c.body
		case "PLTE", "tRNS":
			palette = append(palette, c)
		case "IDAT":
			idat = append(idat, c.body)
		}
	}
	return ihdr, palette, idat
}

// testPixels 是测试图像的原始像素字节。
func testPixels(t *testing.T, img image.Image) []byte {
	t.Helper()
	switch img := img.(type) {
	case *image.NRGBA:
		return img.Pix
	case *image.Paletted:
		return img.Pix
	}
	t.Fatalf("unexpected image type %T", img)
	return nil
}

func fcTL(sequence uint32, frame apngTestFrame, dispose, blend byte) testChunk {
	b := frame.img.Bounds()
	body := binary.BigEndian.AppendUint32(nil, sequence)
	for _, v := range []uint32{uint32(b.Dx()), uint32(b.Dy()), frame.x, frame.y} {
		body = binary.BigEndian.AppendUint32(body, v)
	}
	return testChunk{"fcTL", append(body, 0, 1, 0, 10, dispose, blend)}
}

// apngTestChunks 按 APNG 规范把 frames 拼成块序列，第一帧覆盖整张画布。thumbnail 为 true 时默认图另用 frames[0] 的像素、不属于动画，否则默认图就是第一帧。declared 是 acTL 声明的帧数。
func apngTestChunks(t *testing.T, frames []apngTestFrame, thumbnail bool, declared uint32) []testChunk {
	t.Helper()
	ihdr, palette, idat := encodeTestFrame(t, frames[0].img)
	chunks := append(append([]testChunk{{"IHDR", ihdr}}, palette...), testChunk{"acTL", binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, declared), 0)})
	sequence := uint32(0)
	first := 0
	if !thumbnail {
		chunks = append(chunks, fcTL(sequence, frames[0], 0, 0))
		sequence++
		first = 1
	}
	for _, data := range idat {
		chunks = append(chunks, testChunk{"IDAT", data})
	}
	for _, frame := range frames[first:] {
		chunks = append(chunks, fcTL(sequence, frame, 1, 1))
		sequence++
		_, _, data := encodeTestFrame(t, frame.img)
		for _, part := range data {
			chunks = append(chunks, testChunk{"fdAT", append(binary.BigEndian.AppendUint32(nil, sequence), part...)})
			sequence++
		}
	}
	return append(chunks, testChunk{"IEND", nil})
}

// apngPalettedFrames 与 apngTestFrames 尺寸相同，但都是共用一张调色板（第一个颜色透明）的 Paletted，image/png 编码出 4 位的 PLTE 和 tRNS。
func apngPalettedFrames(count int) []apngTestFrame {
	palette := color.Palette{color.NRGBA{}, color.NRGBA{R: 255, A: 255}, color.NRGBA{G: 255, A: 255}, color.NRGBA{B: 255, A: 255}, color.NRGBA{R: 9, G: 9, B: 9, A: 90}}
	frames := apngTestFrames(count)
	for i, frame := range frames {
		img := image.NewPaletted(frame.img.Bounds(), palette)
		for j := range img.Pix {
			img.Pix[j] = uint8((j + i) % len(palette))
		}
		frames[i].img = img
	}
	return frames
}

func apngTestFrames(count int) []apngTestFrame {
	frames := []apngTestFrame{{img: apngTestImage(12, 10, 0)}}
	for i := 1; i < count; i++ {
		frames = append(frames, apngTestFrame{img: apngTestImage(6+i%3, 4+i%2, i), x: uint32(i % 4), y: uint32(i % 5)})
	}
	return frames
}

// candidateAPNGFixture 是一张三帧、默认图即第一帧的 APNG。
func candidateAPNGFixture(t *testing.T) []byte {
	t.Helper()
	frames := apngTestFrames(3)
	return testPNG(apngTestChunks(t, frames, false, uint32(len(frames))))
}

// decodedAPNGFrames 用 parseCandidateAPNG 和每帧的独立 PNG 解码出全部动画帧，不含不属于动画的默认图。
func decodedAPNGFrames(t *testing.T, data []byte) []image.Image {
	t.Helper()
	a, code := parseCandidateAPNG(data)
	if code != "" {
		t.Fatal(code)
	}
	var out []image.Image
	for _, frame := range a.frames {
		img, err := png.Decode(bytes.NewReader(a.framePNG(frame, nil)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, img)
	}
	return out
}

func TestCandidateAPNGKeepsEveryFrame(t *testing.T) {
	for _, tc := range []struct {
		name      string
		frames    []apngTestFrame
		thumbnail bool
	}{
		{"rgba", apngTestFrames(4), false},
		{"rgba with a separate default image", apngTestFrames(4), true},
		{"paletted with transparency", apngPalettedFrames(4), false},
	} {
		frames, thumbnail := tc.frames, tc.thumbnail
		chunks := apngTestChunks(t, frames, thumbnail, uint32(len(frames)))
		original := testPNG(chunks)
		// 元数据、色彩和未知的辅助块分别插在 IHDR 之后和两帧之间，清理后都不能留下。
		metadata := []testChunk{{"tEXt", []byte("Comment\x00secret-text")}, {"iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00secret-xmp")}, {"zTXt", []byte("k\x00\x00secret-z")}, {"eXIf", []byte("MM\x00*secret-exif")}, {"iCCP", []byte("secret-icc\x00\x00x")}, {"tIME", []byte{7, 234, 10, 16, 0, 0, 0}}, {"gAMA", []byte{0, 0, 177, 143}}, {"pHYs", make([]byte, 9)}, {"prVt", []byte("secret-private")}}
		tagged := append([]testChunk{chunks[0]}, metadata[:5]...)
		for i, c := range chunks[1:] {
			if c.kind == "fcTL" && i > 3 {
				tagged = append(tagged, metadata[5:]...)
			}
			tagged = append(tagged, c)
		}
		// IEND 之后的字节与静态 PNG 一样被丢掉。
		input := append(testPNG(tagged), []byte("trailing-secret")...)
		manifest, files := candidateFixture(t, "shared")
		// 动画装饰图配 schema 2 的待机动效，与客户端写出的包相同。
		manifest = strings.NewReplacer("schema_version = 1", "schema_version = 2", "image = 'assets/deco.jpg'", "image = 'assets/deco.png'", "width_dip = 40\n", "width_dip = 40\nmotion = 'float'\nmotion_period_ms = 2400\n").Replace(manifest)
		delete(files, "assets/deco.jpg")
		files["assets/deco.png"] = input
		if code := candidatePipeline(manifest, files); code != "" {
			t.Fatal("animated decoration refused", tc.name, code)
		}
		clean, code := sanitizeCandidateImages(files, "preview.png")
		if code != "" {
			t.Fatal(code)
		}
		got := clean["assets/deco.png"]
		if !bytes.Equal(got, original) {
			t.Fatal("cleaned APNG is not the original without its metadata", tc.name)
		}
		if strings.Contains(string(got), "secret") {
			t.Fatal("metadata kept")
		}
		decoded := decodedAPNGFrames(t, got)
		if len(decoded) != len(frames) {
			t.Fatalf("decoded %d frames, want %d", len(decoded), len(frames))
		}
		for i, frame := range frames {
			if !bytes.Equal(testPixels(t, decoded[i]), testPixels(t, frame.img)) || decoded[i].Bounds() != frame.img.Bounds() {
				t.Fatalf("%s: frame %d changed", tc.name, i)
			}
		}
		// image/png 仍把它当作普通 PNG 读出默认图，不认识 APNG 的宿主照常画第一帧。
		still, err := png.Decode(bytes.NewReader(got))
		if err != nil || !bytes.Equal(testPixels(t, still), testPixels(t, frames[0].img)) {
			t.Fatal("default image unreadable", err)
		}
	}
}

func TestCandidateAPNGRejectsMalformedFiles(t *testing.T) {
	frames := apngTestFrames(3)
	base := apngTestChunks(t, frames, false, 3)
	index := func(chunks []testChunk, kind string, n int) int {
		for i, c := range chunks {
			if c.kind == kind {
				if n == 0 {
					return i
				}
				n--
			}
		}
		t.Fatalf("no %s #%d", kind, n)
		return -1
	}
	edit := func(change func([]testChunk) []testChunk) []byte {
		chunks := make([]testChunk, len(base))
		for i, c := range base {
			chunks[i] = testChunk{c.kind, append([]byte{}, c.body...)}
		}
		return testPNG(change(chunks))
	}
	valid := testPNG(base)
	badCRC := append([]byte{}, valid...)
	badCRC[len(badCRC)-20] ^= 1
	tooMany := apngTestFrames(maxCandidateAnimationFrames + 1)
	atLimit := apngTestFrames(maxCandidateAnimationFrames)
	for i := range atLimit {
		atLimit[i] = apngTestFrame{img: apngTestImage(2, 2, i)}
		tooMany[i] = atLimit[i]
	}
	tooMany[maxCandidateAnimationFrames] = apngTestFrame{img: apngTestImage(2, 2, 7)}
	if _, code := parseCandidateAPNG(testPNG(apngTestChunks(t, atLimit, false, maxCandidateAnimationFrames))); code != "" {
		t.Fatal("120 frames refused", code)
	}
	for name, tc := range map[string]struct {
		data []byte
		code string
	}{
		"crc mismatch":   {badCRC, "candidate_skin_image_invalid"},
		"truncated":      {valid[:len(valid)*2/3], "candidate_skin_image_invalid"},
		"missing IEND":   {valid[:len(valid)-12], "candidate_skin_image_invalid"},
		"too many":       {testPNG(apngTestChunks(t, tooMany, false, maxCandidateAnimationFrames+1)), "candidate_skin_too_large"},
		"fewer declared": {testPNG(apngTestChunks(t, frames, false, 2)), "candidate_skin_image_invalid"},
		"more declared":  {testPNG(apngTestChunks(t, frames, false, 4)), "candidate_skin_image_invalid"},
		"zero frames":    {edit(func(c []testChunk) []testChunk { binary.BigEndian.PutUint32(c[1].body, 0); return c }), "candidate_skin_image_invalid"},
		"region outside canvas": {edit(func(c []testChunk) []testChunk {
			binary.BigEndian.PutUint32(c[index(c, "fcTL", 1)].body[12:], 12)
			return c
		}), "candidate_skin_image_invalid"},
		"default frame smaller than canvas": {edit(func(c []testChunk) []testChunk {
			binary.BigEndian.PutUint32(c[index(c, "fcTL", 0)].body[4:], 11)
			return c
		}), "candidate_skin_image_invalid"},
		"empty region": {edit(func(c []testChunk) []testChunk {
			binary.BigEndian.PutUint32(c[index(c, "fcTL", 2)].body[8:], 0)
			return c
		}), "candidate_skin_image_invalid"},
		"bad dispose op": {edit(func(c []testChunk) []testChunk { c[index(c, "fcTL", 1)].body[24] = 3; return c }), "candidate_skin_image_invalid"},
		"bad blend op":   {edit(func(c []testChunk) []testChunk { c[index(c, "fcTL", 1)].body[25] = 2; return c }), "candidate_skin_image_invalid"},
		"sequence gap": {edit(func(c []testChunk) []testChunk {
			binary.BigEndian.PutUint32(c[index(c, "fdAT", 0)].body, 9)
			return c
		}), "candidate_skin_image_invalid"},
		"frame without data": {edit(func(c []testChunk) []testChunk {
			i := index(c, "fdAT", 0)
			return append(c[:i:i], c[i+1:]...)
		}), "candidate_skin_image_invalid"},
		"fdAT for the default frame": {edit(func(c []testChunk) []testChunk {
			i := index(c, "fcTL", 1)
			return append(c[:i:i], c[i+1:]...)
		}), "candidate_skin_image_invalid"},
		"IDAT split by another chunk": {edit(func(c []testChunk) []testChunk {
			i := index(c, "IDAT", 0)
			return append(c[:i+1:i+1], append([]testChunk{{"tEXt", []byte("a\x00b")}, c[i]}, c[i+1:]...)...)
		}), "candidate_skin_image_invalid"},
		"unknown critical chunk": {edit(func(c []testChunk) []testChunk {
			return append([]testChunk{c[0], {"QxQx", []byte{1}}}, c[1:]...)
		}), "candidate_skin_image_invalid"},
		"PLTE after IDAT": {edit(func(c []testChunk) []testChunk {
			i := index(c, "fcTL", 1)
			return append(c[:i:i], append([]testChunk{{"PLTE", []byte{0, 0, 0}}}, c[i:]...)...)
		}), "candidate_skin_image_invalid"},
		"IEND with data": {edit(func(c []testChunk) []testChunk { c[len(c)-1].body = []byte{0}; return c }), "candidate_skin_image_invalid"},
		"chunk type with a digit": {edit(func(c []testChunk) []testChunk {
			return append([]testChunk{c[0], {"te1t", []byte{1}}}, c[1:]...)
		}), "candidate_skin_image_invalid"},
		// 结构与 CRC 都合法、只有一帧的压缩数据截断：只有逐帧解码才能发现。
		"corrupt frame data": {edit(func(c []testChunk) []testChunk {
			i := index(c, "fdAT", 1)
			c[i].body = c[i].body[:len(c[i].body)/2]
			return c
		}), "candidate_skin_image_invalid"},
		"extra data after a frame": {edit(func(c []testChunk) []testChunk {
			i := index(c, "fdAT", 0)
			c[i].body = append(c[i].body, 1, 2, 3)
			return c
		}), "candidate_skin_image_invalid"},
		"extra fdAT after the last frame": {edit(func(c []testChunk) []testChunk {
			last := c[len(c)-2]
			junk := append(binary.BigEndian.AppendUint32(nil, binary.BigEndian.Uint32(last.body)+1), []byte("hidden-payload")...)
			return append(c[:len(c)-1:len(c)-1], testChunk{"fdAT", junk}, c[len(c)-1])
		}), "candidate_skin_image_invalid"},
		"extra IDAT after the default image": {edit(func(c []testChunk) []testChunk {
			i := index(c, "IDAT", 0)
			return append(c[:i+1:i+1], append([]testChunk{{"IDAT", []byte("hidden-payload")}}, c[i+1:]...)...)
		}), "candidate_skin_image_invalid"},
		"corrupt default image": {edit(func(c []testChunk) []testChunk {
			i := index(c, "IDAT", 0)
			c[i].body = c[i].body[:len(c[i].body)-6]
			return c
		}), "candidate_skin_image_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if !candidateAnimatedPNG(tc.data) {
				t.Fatal("fixture is not detected as APNG")
			}
			if _, code := sanitizeCandidateImages(map[string][]byte{"a.png": tc.data}, ""); code != tc.code {
				t.Fatalf("got %q want %q", code, tc.code)
			}
		})
	}
	// acTL 出现在 IDAT 之后的文件按规范不是 APNG，走静态 PNG 的重新编码，只保留默认图。
	i := index(base, "IDAT", 0)
	moved := append(append(append([]testChunk{base[0]}, base[2:i+1]...), base[1]), base[i+1:]...)
	data := testPNG(moved)
	if candidateAnimatedPNG(data) {
		t.Fatal("acTL after IDAT detected as APNG")
	}
	if clean, code := sanitizeCandidateImages(map[string][]byte{"a.png": data}, ""); code != "" || bytes.Contains(clean["a.png"], []byte("acTL")) {
		t.Fatal("acTL after IDAT not re-encoded as a still image", code)
	}
}

func TestCandidateAPNGPixelBudget(t *testing.T) {
	// 预算在解码之前从文件头计算：把画布和第一帧改成 1700×1700，三幅画面合计约 867 万像素，超过 800 万；两帧时约 578 万，可以通过预算检查（像素数据随后解码失败，说明预算检查在前）。
	frames := apngTestFrames(3)
	resize := func(count int) []byte {
		chunks := apngTestChunks(t, frames[:count], false, uint32(count))
		for _, i := range []int{0, 2} {
			offset := 0
			if chunks[i].kind == "fcTL" {
				offset = 4
			}
			binary.BigEndian.PutUint32(chunks[i].body[offset:], 1700)
			binary.BigEndian.PutUint32(chunks[i].body[offset+4:], 1700)
		}
		return testPNG(chunks)
	}
	if _, code := sanitizeCandidateImages(map[string][]byte{"a.png": resize(3)}, ""); code != "candidate_skin_too_large" {
		t.Fatal("three 1700×1700 images accepted", code)
	}
	if _, code := sanitizeCandidateImages(map[string][]byte{"a.png": resize(2)}, ""); code != "candidate_skin_image_invalid" {
		t.Fatal("two 1700×1700 images should pass the budget and fail decoding", code)
	}
	// 默认图不属于动画时多算一幅：两帧加默认图的 1500×1500 画布是 675 万，再加一张 1200×1100 的静态图（132 万）就超过上限；只算两帧时是 582 万，不会超过。
	thumbnail := apngTestChunks(t, frames[:2], true, 2)
	binary.BigEndian.PutUint32(thumbnail[0].body, 1500)
	binary.BigEndian.PutUint32(thumbnail[0].body[4:], 1500)
	a, code := parseCandidateAPNG(testPNG(thumbnail))
	if code != "" || a.images != 3 {
		t.Fatal("thumbnail counted", code)
	}
	files := map[string][]byte{"a.png": testPNG(thumbnail), "b.png": candidatePNG(t, 1200, 1100)}
	if _, code := sanitizeCandidateImages(files, ""); code != "candidate_skin_too_large" {
		t.Fatal("budget ignores the thumbnail", code)
	}
	// 画布边长仍受 2048 限制。
	wide := apngTestChunks(t, frames, false, 3)
	binary.BigEndian.PutUint32(wide[0].body, 2049)
	binary.BigEndian.PutUint32(wide[2].body[4:], 2049)
	if _, code := sanitizeCandidateImages(map[string][]byte{"a.png": testPNG(wide)}, ""); code != "candidate_skin_too_large" {
		t.Fatal("canvas over 2048 accepted", code)
	}
}

func TestCandidateAnimatedPNGDetection(t *testing.T) {
	static := candidatePNG(t, 4, 4)
	for name, data := range map[string][]byte{
		"static":          static,
		"jpeg":            candidateJPEG(t, 4, 4),
		"empty":           nil,
		"signature only":  pngSignature,
		"overlong length": append(append([]byte{}, pngSignature...), 0x7F, 0, 0, 0, 'a', 'b', 'c', 'd', 0, 0, 0, 0),
	} {
		if candidateAnimatedPNG(data) {
			t.Fatal(name, "detected as APNG")
		}
	}
	if !candidateAnimatedPNG(candidateAPNGFixture(t)) {
		t.Fatal("APNG not detected")
	}
	// 普通 PNG 仍然解码后重新编码，结果与之前相同。
	clean, code := sanitizeCandidateImages(map[string][]byte{"a.png": static}, "")
	decoded, err := png.Decode(bytes.NewReader(static))
	var want bytes.Buffer
	if err == nil {
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&want, decoded)
	}
	if code != "" || err != nil || !bytes.Equal(clean["a.png"], want.Bytes()) {
		t.Fatal("static PNG path changed", code, err)
	}
	if parsed, code := parseCandidateAPNG(static); parsed != nil || code != "candidate_skin_image_invalid" {
		t.Fatal("static PNG parsed as APNG", code)
	}
	if _, code := parseCandidateAPNG([]byte("not a png")); code != "candidate_skin_image_invalid" {
		t.Fatal("garbage parsed", code)
	}
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE([]byte("IHDR")))
	if _, code := parseCandidateAPNG(append(append(append([]byte{}, pngSignature...), 0, 0, 0, 0, 'I', 'H', 'D', 'R'), crc[:]...)); code != "candidate_skin_image_invalid" {
		t.Fatal("empty IHDR parsed", code)
	}
}

// TestCommunityCandidateSkinKeepsAnimation 走完发布、预览和下载：装饰图和预览图都是 APNG，入库与下发的都是去掉元数据后的完整动画。
func TestCommunityCandidateSkinKeepsAnimation(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "candidate-apng@example.test"})
	frames := apngTestFrames(3)
	chunks := apngTestChunks(t, frames, false, 3)
	animated := testPNG(chunks)
	tagged := testPNG(append([]testChunk{chunks[0], {"tEXt", []byte("Comment\x00secret-text")}}, chunks[1:]...))
	manifest, _ := candidateFixture(t, "animated")
	manifest = strings.NewReplacer("schema_version = 1", "schema_version = 2", "image = 'assets/deco.jpg'", "image = 'assets/deco.png'", "width_dip = 40\n", "width_dip = 40\nmotion = 'bounce'\n").Replace(manifest)
	files := map[string][]byte{"preview.png": tagged, "assets/deco.png": tagged}
	id := "a9334455-1234-4234-8234-123456789abc"
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, id, "动画皮肤", manifest, files), owner.AccessToken, 201)

	w := apiRequest(t, mux, "GET", "/v1/community/candidate-skins/"+id+"/preview", "", "", 200)
	var preview struct {
		ContentType string `json:"content_type"`
		Data        []byte `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview.ContentType != "image/png" || !bytes.Equal(preview.Data, animated) {
		t.Fatal("preview is not the cleaned APNG", preview.ContentType, err)
	}
	w = apiRequest(t, mux, "POST", "/v1/community/candidate-skins/"+id+"/download", "", "", 200)
	var download struct {
		Manifest string            `json:"manifest"`
		Files    map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &download); err != nil || download.Manifest != manifest || len(download.Files) != 2 {
		t.Fatal(w.Body.String(), err)
	}
	for path, data := range download.Files {
		if !bytes.Equal(data, animated) {
			t.Fatal(path, "is not the cleaned APNG")
		}
	}
	if len(decodedAPNGFrames(t, download.Files["assets/deco.png"])) != len(frames) {
		t.Fatal("frames lost")
	}
}
