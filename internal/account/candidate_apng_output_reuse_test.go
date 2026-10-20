package account

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"maps"
	"math/rand/v2"
	"testing"
)

// 保留本轮改动前的完整清理流程，用于比较输出和分配开销。
func previousSanitizeCandidateAPNGOutputImages(files map[string][]byte, preview string) (map[string][]byte, string) {
	pixels := 0
	animated := map[string]*candidateAPNG{}
	for path, data := range files {
		var config image.Config
		var err error
		images := 1
		if candidateExtension(path) == "png" && candidateAnimatedPNG(data) {
			a, code := parseCandidateAPNG(data)
			if code != "" {
				return nil, code
			}
			animated[path] = a
			config.Width, config.Height, images = a.width, a.height, a.images
		} else if candidateExtension(path) == "png" {
			config, err = png.DecodeConfig(bytes.NewReader(data))
		} else if jpegScans(data) > maxJPEGScans {
			return nil, "candidate_skin_image_invalid"
		} else {
			config, err = jpeg.DecodeConfig(bytes.NewReader(data))
		}
		if err != nil {
			return nil, "candidate_skin_image_invalid"
		}
		if config.Width < 1 || config.Height < 1 || config.Width > maxCandidateSide || config.Height > maxCandidateSide {
			return nil, "candidate_skin_too_large"
		}
		pixels += config.Width * config.Height * images
		if pixels > maxCandidatePixels {
			return nil, "candidate_skin_too_large"
		}
	}
	clean := make(map[string][]byte, len(files))
	total := 0
	var pngBuffers candidatePNGBufferPool
	pngEncoder := png.Encoder{CompressionLevel: png.BestCompression, BufferPool: &pngBuffers}
	for path, data := range files {
		var decoded image.Image
		var err error
		var out bytes.Buffer
		if a := animated[path]; a != nil {
			if !a.decode() {
				return nil, "candidate_skin_image_invalid"
			}
			out.Write(a.clean)
		} else if candidateExtension(path) == "png" {
			if decoded, err = png.Decode(bytes.NewReader(data)); err == nil {
				err = pngEncoder.Encode(&out, decoded)
			}
		} else {
			if decoded, err = jpeg.Decode(bytes.NewReader(data)); err == nil {
				err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 90})
			}
		}
		if err != nil {
			return nil, "candidate_skin_image_invalid"
		}
		total += out.Len()
		if out.Len() > maxCandidateFileBytes || total > maxCandidatePackageBytes || (path == preview && out.Len() > maxCandidatePreviewBytes) {
			return nil, "candidate_skin_too_large"
		}
		clean[path] = out.Bytes()
	}
	return clean, ""
}

// 使用真实灰度像素和独立帧压缩流，生成尺寸可控、没有可剥离元数据的 APNG。
func candidateAPNGOutputFixture(tb testing.TB, side, count int) []byte {
	tb.Helper()
	output := bytes.Clone(pngSignature)
	sequence := uint32(0)
	random := rand.New(rand.NewPCG(1, 2))
	for frame := range count {
		img := image.NewGray(image.Rect(0, 0, side, side))
		for i := range img.Pix {
			img.Pix[i] = byte(random.Uint64())
		}
		var encoded bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.NoCompression}
		if err := encoder.Encode(&encoded, img); err != nil {
			tb.Fatal(err)
		}
		data := encoded.Bytes()
		for pos := len(pngSignature); pos < len(data); {
			length := int(binary.BigEndian.Uint32(data[pos:]))
			body := data[pos+8 : pos+8+length]
			switch string(data[pos+4 : pos+8]) {
			case "IHDR":
				if frame == 0 {
					output = appendPNGChunk(output, "IHDR", body)
					control := binary.BigEndian.AppendUint32(nil, uint32(count))
					output = appendPNGChunk(output, "acTL", binary.BigEndian.AppendUint32(control, 0))
				}
				control := fcTL(sequence, apngTestFrame{img: img}, 0, 0)
				output = appendPNGChunk(output, control.kind, control.body)
				sequence++
			case "IDAT":
				if frame == 0 {
					output = appendPNGChunk(output, "IDAT", body)
				} else {
					body = append(binary.BigEndian.AppendUint32(nil, sequence), body...)
					output = appendPNGChunk(output, "fdAT", body)
					sequence++
				}
			}
			pos += length + 12
		}
	}
	return appendPNGChunk(output, "IEND", nil)
}

func TestCandidateAPNGOutputOwnership(t *testing.T) {
	for _, size := range []int{16, 512 << 10} {
		t.Run(fmt.Sprintf("元数据%d字节", size), func(t *testing.T) {
			testCandidateAPNGOutputOwnership(t, size)
		})
	}
}

func testCandidateAPNGOutputOwnership(t *testing.T, metadataSize int) {
	t.Helper()
	input := pngWithChunk(candidateAPNGFixture(t), "tEXt", []byte("Comment\x00synthetic-marker"))
	input = append(pngWithChunk(input, "prVt", bytes.Repeat([]byte{1}, metadataSize)), []byte("synthetic-trailing-marker")...)
	saved := bytes.Clone(input)
	files := map[string][]byte{"decoration.png": input, "background.png": candidatePNG(t, 8, 9), "preview.jpg": candidateJPEG(t, 7, 6)}
	want, wantCode := previousSanitizeCandidateAPNGOutputImages(files, "preview.jpg")
	got, code := sanitizeCandidateImages(files, "preview.jpg")
	if code != "" || code != wantCode || !maps.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("混合图片清理结果改变：实际错误=%q，原错误=%q", code, wantCode)
	}
	if bytes.Contains(got["decoration.png"], []byte("synthetic-marker")) {
		t.Fatal("清理保留了文本元数据")
	}
	if capacity, size := cap(got["decoration.png"]), len(got["decoration.png"]); capacity > 2*size+64 {
		t.Fatalf("清理结果保留了过大缓冲：容量=%d，内容=%d", capacity, size)
	}
	second, code := sanitizeCandidateImages(files, "preview.jpg")
	if code != "" {
		t.Fatal(code)
	}
	got["decoration.png"][0] ^= 1
	if !bytes.Equal(input, saved) || !bytes.Equal(second["decoration.png"], want["decoration.png"]) {
		t.Fatal("修改清理结果影响了上传数据或另一次清理结果")
	}
}

func TestCandidateAPNGOutputSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		side    int
		files   int
		preview string
		code    string
	}{
		{"正常装饰图", 768, 1, "", ""},
		{"预览图超限", 768, 1, "image0.png", "candidate_skin_too_large"},
		{"单文件超限", 1024, 1, "", "candidate_skin_too_large"},
		{"整包超限", 900, 3, "", "candidate_skin_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := candidateAPNGOutputFixture(t, tc.side, 1)
			files := make(map[string][]byte, tc.files)
			for i := range tc.files {
				files[fmt.Sprintf("image%d.png", i)] = data
			}
			if _, code := sanitizeCandidateImages(files, tc.preview); code != tc.code {
				t.Fatalf("大小限制错误：实际=%q，期望=%q", code, tc.code)
			}
		})
	}
}

func TestCandidateAPNGOutputMemoryBudget(t *testing.T) {
	files := map[string][]byte{"decoration.png": candidateAPNGOutputFixture(t, 768, 1)}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, code := sanitizeCandidateImages(files, ""); code != "" {
				b.Fatal(code)
			}
		}
	})
	t.Logf("%s；%s", result, result.MemString())
	// 包含结构检查与全部像素解码，拒绝再复制约 577 KiB 清理结果的回归。
	if allocated := result.AllocedBytesPerOp(); allocated > 2<<20 {
		t.Fatalf("APNG 清理分配 %d 字节，超过 2 MiB 预算", allocated)
	}
}

func BenchmarkCandidateAPNGOutput(b *testing.B) {
	for _, tc := range []struct {
		name        string
		side, count int
	}{
		{"small", 16, 3}, {"medium", 256, 3}, {"large", 768, 1},
	} {
		files := map[string][]byte{"decoration.png": candidateAPNGOutputFixture(b, tc.side, tc.count)}
		for _, implementation := range []struct {
			name string
			run  func(map[string][]byte, string) (map[string][]byte, string)
		}{{"previous", previousSanitizeCandidateAPNGOutputImages}, {"current", sanitizeCandidateImages}} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, code := implementation.run(files, ""); code != "" {
						b.Fatal(code)
					}
				}
			})
		}
	}
}
