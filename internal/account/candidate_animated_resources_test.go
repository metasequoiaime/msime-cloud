package account

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"net/http/httptest"
	"strings"
	"testing"
)

// WebP 样本由本地 Pillow 对 4×3 纯色合成图生成，不含外部素材或用户数据。
var candidateWebPSamples = map[string]string{
	"lossy":       "UklGRjYAAABXRUJQVlA4ICoAAACwAQCdASoEAAMAAUAmJaACdLoABKoAAP4TH/nEv5I/QLuz/7jW3P1iAAA=",
	"lossless":    "UklGRh4AAABXRUJQVlA4TBEAAAAvA4AAAAdQwWpVuv+BiOh/AAA=",
	"alpha-lossy": "UklGRloAAABXRUJQVlA4WAoAAAAQAAAAAwAAAgAAQUxQSAoAAAABB1C8iAhERP8DVlA4ICoAAACwAQCdASoEAAMAAUAmJaACdLoABKoAAP4TH/nEv5I/QLuz/7jW3P1iAAA=",
	"alpha":       "UklGRh4AAABXRUJQVlA4TBEAAAAvA4AAEAdQwWpVuniBiOh/AAA=",
	"animation":   "UklGRogAAABXRUJQVlA4WAoAAAASAAAAAwAAAgAAQU5JTQYAAAAAAAAAAwBBTk1GKgAAAAAAAAAAAAMAAAIAAEYAAAJWUDhMEQAAAC8DgAAAB1DBalW6/4GI6H8AAEFOTUYqAAAAAAAAAAAAAwAAAgAAlgAAAlZQOEwRAAAALwOAABAHUJRy16hkgYjofwAA",
}

func candidateWebPFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(candidateWebPSamples[name])
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func candidateGIFFixture(t *testing.T, frames int) []byte {
	t.Helper()
	palette := color.Palette{color.NRGBA{}, color.NRGBA{R: 100, G: 40, B: 220, A: 255}}
	a := &gif.GIF{LoopCount: 3, Config: image.Config{ColorModel: palette, Width: 4, Height: 3}}
	for i := range frames {
		frame := image.NewPaletted(image.Rect(0, 0, 4, 3), palette)
		frame.SetColorIndex(i%4, i%3, 1)
		a.Image = append(a.Image, frame)
		a.Delay = append(a.Delay, 7+i)
		a.Disposal = append(a.Disposal, byte(2+i%2))
	}
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, a); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestCandidateGIFPreservesAnimationAndStripsMetadata(t *testing.T) {
	original := candidateGIFFixture(t, 3)
	comment := []byte{0x21, 0xfe, 16}
	comment = append(comment, []byte("synthetic-secret")...)
	comment = append(comment, 0)
	// 扩展块插在末尾结束符之前，保留全局调色板的位置。
	data := append(bytes.Clone(original[:len(original)-1]), comment...)
	data = append(data, 0x3b)
	data = append(data, "synthetic-tail"...)
	clean, code := sanitizeCandidateImages(map[string][]byte{"motion.gif": data}, "")
	if code != "" {
		t.Fatal(code)
	}
	if bytes.Contains(clean["motion.gif"], []byte("synthetic-")) {
		t.Fatal("GIF 元数据未清理")
	}
	before, _ := gif.DecodeAll(bytes.NewReader(original))
	after, err := gif.DecodeAll(bytes.NewReader(clean["motion.gif"]))
	if err != nil || len(after.Image) != 3 || after.LoopCount != 3 {
		t.Fatal("GIF 动画丢失", err)
	}
	for i := range before.Image {
		if !bytes.Equal(before.Image[i].Pix, after.Image[i].Pix) || before.Delay[i] != after.Delay[i] || before.Disposal[i] != after.Disposal[i] {
			t.Fatal("GIF 帧、时长或处置改变", i)
		}
	}
}

func TestCandidateWebPResources(t *testing.T) {
	for name := range candidateWebPSamples {
		t.Run(name, func(t *testing.T) {
			original := candidateWebPFixture(t, name)
			// RIFF 顶层未知块和元数据必须从清理后的资源消失。
			data := candidateWebPContainer(candidateWebPAppend(bytes.Clone(original[12:]), "EXIF", []byte("synthetic-secret")))
			clean, code := sanitizeCandidateImages(map[string][]byte{"motion.webp": data}, "")
			if code != "" {
				t.Fatal(code)
			}
			if bytes.Contains(clean["motion.webp"], []byte("synthetic-secret")) {
				t.Fatal("WebP 元数据未清理")
			}
			before, code := parseCandidateWebP(original)
			if code != "" {
				t.Fatal(code)
			}
			after, code := parseCandidateWebP(clean["motion.webp"])
			if code != "" || before.images != after.images || before.width != after.width || before.height != after.height || !after.decode() {
				t.Fatal("WebP 动画或尺寸丢失", code)
			}
			if !bytes.Equal(before.clean, after.clean) {
				t.Fatal("WebP 绘制块或动画控制信息改变")
			}
			if name == "animation" && after.images != 2 {
				t.Fatal("WebP 被压成静态图")
			}
		})
	}
}

func TestCandidateAnimatedResourcesRejectMalformed(t *testing.T) {
	gifData := candidateGIFFixture(t, 2)
	webpData := candidateWebPFixture(t, "animation")
	for ext, data := range map[string][]byte{"gif": gifData, "webp": webpData} {
		t.Run(ext, func(t *testing.T) {
			for i := 0; i < len(data)-1; i++ {
				if _, code := sanitizeCandidateImages(map[string][]byte{"motion." + ext: data[:i]}, ""); code == "" {
					t.Fatalf("截断 %d 被接受", i)
				}
			}
			if _, code := sanitizeCandidateImages(map[string][]byte{"motion." + ext: []byte("synthetic")}, ""); code != "candidate_skin_image_invalid" {
				t.Fatal(code)
			}
		})
	}
	hugeGIF := candidateGIFFixture(t, 2)
	binary.LittleEndian.PutUint16(hugeGIF[6:], 2048)
	binary.LittleEndian.PutUint16(hugeGIF[8:], 2048)
	if _, code := sanitizeCandidateImages(map[string][]byte{"motion.gif": hugeGIF}, ""); code != "candidate_skin_too_large" {
		t.Fatal(code)
	}
	manyGIF := candidateGIFFixture(t, maxCandidateAnimationFrames+1)
	if _, code := sanitizeCandidateImages(map[string][]byte{"motion.gif": manyGIF}, ""); code != "candidate_skin_too_large" {
		t.Fatal(code)
	}
	chunks, _ := candidateWebPChunks(webpData[12:])
	var repeated []byte
	for _, chunk := range chunks {
		if chunk.kind != "ANMF" {
			repeated = candidateWebPAppend(repeated, chunk.kind, chunk.body)
		}
	}
	for range maxCandidateAnimationFrames + 1 {
		repeated = candidateWebPAppend(repeated, "ANMF", chunks[2].body)
	}
	if _, code := sanitizeCandidateImages(map[string][]byte{"motion.webp": candidateWebPContainer(repeated)}, ""); code != "candidate_skin_too_large" {
		t.Fatal(code)
	}
	for _, kind := range []string{"ANMF", "VP8X"} {
		var malformed []byte
		for _, chunk := range chunks {
			body := bytes.Clone(chunk.body)
			if chunk.kind == kind {
				body = body[:len(body)-1]
			}
			malformed = candidateWebPAppend(malformed, chunk.kind, body)
		}
		if _, code := sanitizeCandidateImages(map[string][]byte{"motion.webp": candidateWebPContainer(malformed)}, ""); code == "" {
			t.Fatal("损坏 WebP 被接受", kind)
		}
	}
	// 后续帧的压缩位流必须真正解码，不能只验证帧头。
	var broken []byte
	for i, chunk := range chunks {
		body := bytes.Clone(chunk.body)
		if i == len(chunks)-1 {
			clear(body[29:])
		}
		broken = candidateWebPAppend(broken, chunk.kind, body)
	}
	if _, code := sanitizeCandidateImages(map[string][]byte{"motion.webp": candidateWebPContainer(broken)}, ""); code == "" {
		t.Fatal("WebP 后续损坏帧未解码")
	}
}

func TestCandidateResourceNegotiation(t *testing.T) {
	for _, query := range []string{"", "?resources=canvas-v1", "?resources=unknown"} {
		w := httptest.NewRecorder()
		extended, ok := candidateResources(w, httptest.NewRequest("GET", "/"+query, nil).URL.Query(), true)
		if ok != (query != "?resources=unknown") || extended != (query == "?resources=canvas-v1") {
			t.Fatal(query)
		}
		if ok && w.Header().Get(candidateResourcesHeader) != candidateResourcesVersion {
			t.Fatal("缺少服务端能力声明")
		}
	}
	files := map[string][]byte{}
	for i := range maxCandidateFiles {
		files[strings.Repeat("a", i+1)+".png"] = []byte{1}
	}
	if code := validCandidateResourceFiles("synthetic", files, true); code != "" {
		t.Fatal(code)
	}
	if code := validCandidateFiles("synthetic", files); code != "candidate_skin_file_path" {
		t.Fatal(code)
	}
	files["one-more.png"] = []byte{1}
	if code := validCandidateResourceFiles("synthetic", files, true); code != "candidate_skin_file_path" {
		t.Fatal(code)
	}
	for _, path := range []string{"motion.gif", "motion.webp", "motion.apng"} {
		files := map[string][]byte{path: {1}}
		if code := validCandidateResourceFiles("synthetic", files, true); code != "" {
			t.Fatal(path, code)
		}
		if code := validCandidateFiles("synthetic", files); code != "candidate_skin_file_type" {
			t.Fatal(path, code)
		}
	}
}

// 开关关闭时仍可读取已存在的扩展包，但不承诺或接受新的扩展上传。
func TestCandidateResourcesDisabledDoesNotAdvertiseOrEnableUploads(t *testing.T) {
	w := httptest.NewRecorder()
	extended, ok := candidateResources(w, httptest.NewRequest("GET", "/?resources=canvas-v1", nil).URL.Query(), false)
	if !ok || !extended || w.Header().Get(candidateResourcesHeader) != "" {
		t.Fatal("关闭时错误声明能力或拒绝扩展读取")
	}
	for _, enabled := range []bool{false, true} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/?resources=canvas-v1", strings.NewReader(`{"name":"合成","manifest":"synthetic","files":{}}`))
		r.Header.Set("Content-Type", "application/json")
		var input candidateUpload
		_, ok := readCandidateUpload(w, r, &input, enabled)
		if !ok || input.extendedResources != enabled {
			t.Fatal("上传开关未生效", enabled, w.Body.String())
		}
	}
}
