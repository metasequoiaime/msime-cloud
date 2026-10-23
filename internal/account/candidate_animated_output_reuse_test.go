package account

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"maps"
	"math/rand/v2"
	"testing"
)

// 保留改动前的完整清理流程，比较输出与分配开销。
func previousSanitizeCandidateAnimatedOutputImages(files map[string][]byte, preview string) (map[string][]byte, string) {
	pixels := 0
	animated := map[string]*candidateAPNG{}
	webps := map[string]*candidateWebP{}
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
		} else if candidateExtension(path) == "gif" {
			var code string
			config.Width, config.Height, images, code = candidateGIFConfig(data)
			if code != "" {
				return nil, code
			}
		} else if candidateExtension(path) == "webp" {
			a, code := parseCandidateWebP(data)
			if code != "" {
				return nil, code
			}
			webps[path] = a
			config.Width, config.Height, images = a.width, a.height, a.images
		} else if candidateExtension(path) != "jpeg" {
			return nil, "candidate_skin_image_invalid"
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
			// 解析结果已独立于上传字节；剥离大部分内容时仍压紧容量，避免保留过大的底层数组。
			if cap(a.clean) > 2*len(a.clean) {
				out.Write(a.clean)
			} else {
				out = *bytes.NewBuffer(a.clean)
			}
		} else if a := webps[path]; a != nil {
			if !a.decode() {
				return nil, "candidate_skin_image_invalid"
			}
			out.Write(a.clean)
		} else if candidateExtension(path) == "gif" {
			var clean []byte
			clean, err = cleanCandidateGIF(data)
			out.Write(clean)
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

// 使用确定性的合成调色板噪声生成较大 GIF，避免外部素材或压缩率失真的纯色样本。
func candidateGIFOutputFixture(tb testing.TB, side, frames int) []byte {
	tb.Helper()
	random := rand.New(rand.NewPCG(3, 4))
	animation := &gif.GIF{LoopCount: 3, Config: image.Config{ColorModel: color.Palette(palette.Plan9), Width: side, Height: side}}
	for i := range frames {
		frame := image.NewPaletted(image.Rect(0, 0, side, side), palette.Plan9)
		for j := range frame.Pix {
			frame.Pix[j] = byte(random.Uint64())
		}
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, i+7)
		animation.Disposal = append(animation.Disposal, byte(2+i%2))
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, animation); err != nil {
		tb.Fatal(err)
	}
	return out.Bytes()
}

// 复用已有 4×3 合成 WebP 的编码帧，构造可完整解码的 120 帧动画。
func candidateWebPOutputFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	data, err := base64.StdEncoding.DecodeString(candidateWebPSamples[name])
	if err != nil {
		tb.Fatal(err)
	}
	if name != "animation" {
		return data
	}
	chunks, ok := candidateWebPChunks(data[12:])
	if !ok {
		tb.Fatal("合成 WebP 块无效")
	}
	var content []byte
	for _, chunk := range chunks {
		if chunk.kind != "ANMF" {
			content = candidateWebPAppend(content, chunk.kind, chunk.body)
		}
	}
	for range maxCandidateAnimationFrames / 2 {
		for _, chunk := range chunks {
			if chunk.kind == "ANMF" {
				content = candidateWebPAppend(content, chunk.kind, chunk.body)
			}
		}
	}
	return candidateWebPContainer(content)
}

func TestCandidateAnimatedOutputOwnership(t *testing.T) {
	for _, metadataSize := range []int{16, 512 << 10} {
		t.Run(fmt.Sprint(metadataSize), func(t *testing.T) {
			gifData := candidateGIFFixture(t, 3)
			gifData = append(gifData, bytes.Repeat([]byte{1}, metadataSize)...)
			files := map[string][]byte{
				"motion.gif": gifData, "other.gif": gifData,
				"background.png": candidatePNG(t, 8, 9), "preview.jpg": candidateJPEG(t, 7, 6),
				"decoration.png": candidateAPNGFixture(t),
			}
			for name := range candidateWebPSamples {
				data := candidateWebPFixture(t, name)
				data = candidateWebPContainer(candidateWebPAppend(bytes.Clone(data[12:]), "EXIF", bytes.Repeat([]byte{1}, metadataSize)))
				files[name+".webp"], files["other-"+name+".webp"] = data, data
			}
			saved := make(map[string][]byte, len(files))
			for name, data := range files {
				saved[name] = bytes.Clone(data)
			}
			want, wantCode := previousSanitizeCandidateAnimatedOutputImages(files, "preview.jpg")
			got, code := sanitizeCandidateImages(files, "preview.jpg")
			if code != "" || code != wantCode || !maps.EqualFunc(got, want, bytes.Equal) {
				t.Fatalf("混合资源输出改变：实际错误=%q，原错误=%q", code, wantCode)
			}
			second, code := sanitizeCandidateImages(files, "preview.jpg")
			if code != "" {
				t.Fatal(code)
			}
			for name, data := range got {
				if capacity, size := cap(data), len(data); capacity > 2*size+64 {
					t.Fatalf("%s 保留过大缓冲：容量=%d，内容=%d", name, capacity, size)
				}
				data[0] ^= 1
				for other, output := range got {
					if other != name && !bytes.Equal(output, want[other]) {
						t.Fatalf("修改 %s 影响了 %s", name, other)
					}
				}
				if !maps.EqualFunc(files, saved, bytes.Equal) || !maps.EqualFunc(second, want, bytes.Equal) {
					t.Fatalf("修改 %s 影响了输入或下一次清理", name)
				}
				data[0] ^= 1
			}
		})
	}
}

func TestCandidateAnimatedOutputSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		side, count   int
		preview, code string
	}{
		{"正常装饰图", 512, 1, "", ""},
		{"预览图超限", 512, 1, "image0.gif", "candidate_skin_too_large"},
		{"单文件超限", 1024, 1, "", "candidate_skin_too_large"},
		{"整包超限", 768, 3, "", "candidate_skin_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := candidateGIFOutputFixture(t, tc.side, 1)
			files := make(map[string][]byte, tc.count)
			for i := range tc.count {
				files[fmt.Sprintf("image%d.gif", i)] = data
			}
			_, wantCode := previousSanitizeCandidateAnimatedOutputImages(files, tc.preview)
			if _, code := sanitizeCandidateImages(files, tc.preview); code != tc.code || code != wantCode {
				t.Fatalf("大小限制改变：实际=%q，原错误=%q，期望=%q", code, wantCode, tc.code)
			}
		})
	}
}

func candidateAnimatedOutputCases(tb testing.TB) map[string]map[string][]byte {
	tb.Helper()
	return map[string]map[string][]byte{
		"gif-static":     {"motion.gif": candidateGIFOutputFixture(tb, 512, 1)},
		"gif-animation":  {"motion.gif": candidateGIFOutputFixture(tb, 512, 2)},
		"webp-static":    {"motion.webp": candidateWebPOutputFixture(tb, "alpha-lossy")},
		"webp-animation": {"motion.webp": candidateWebPOutputFixture(tb, "animation")},
	}
}

func TestCandidateAnimatedOutputMemoryBudget(t *testing.T) {
	for name, files := range candidateAnimatedOutputCases(t) {
		t.Run(name, func(t *testing.T) {
			clean, code := sanitizeCandidateImages(files, "")
			if code != "" {
				t.Fatal(code)
			}
			var results [2]testing.BenchmarkResult
			for i, run := range []func(map[string][]byte, string) (map[string][]byte, string){previousSanitizeCandidateAnimatedOutputImages, sanitizeCandidateImages} {
				results[i] = testing.Benchmark(func(b *testing.B) {
					for b.Loop() {
						if _, code := run(files, ""); code != "" {
							b.Fatal(code)
						}
					}
				})
			}
			var outputSize int
			for _, data := range clean {
				outputSize += len(data)
			}
			t.Logf("原流程：%s；当前：%s；输出：%d 字节", results[0].MemString(), results[1].MemString(), outputSize)
			// 完整清理仍需解码全部帧，但不应再复制输出；留出一成余量容纳测量波动。
			for _, data := range clean {
				if saved := results[0].AllocedBytesPerOp() - results[1].AllocedBytesPerOp(); saved < int64(len(data))*9/10 {
					t.Fatalf("未消除完整复制：节省=%d，输出=%d", saved, len(data))
				}
			}
		})
	}
}

func BenchmarkCandidateAnimatedOutput(b *testing.B) {
	for name, files := range candidateAnimatedOutputCases(b) {
		for _, implementation := range []struct {
			name string
			run  func(map[string][]byte, string) (map[string][]byte, string)
		}{
			{"previous", previousSanitizeCandidateAnimatedOutputImages}, {"current", sanitizeCandidateImages},
		} {
			b.Run(name+"/"+implementation.name, func(b *testing.B) {
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
