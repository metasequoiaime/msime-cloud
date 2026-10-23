package account

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"maps"
	"testing"
)

// 保留每张静态 PNG 单独创建编码工作区的实现，比较完整图片清理流程。
func previousSanitizeCandidatePNGImages(files map[string][]byte, preview string) (map[string][]byte, string) {
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
				err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, decoded)
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

func candidatePNGReuseEncode(tb testing.TB, img image.Image) []byte {
	tb.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		tb.Fatal(err)
	}
	return out.Bytes()
}

func candidatePNGReuseFixture(tb testing.TB, count, side int) map[string][]byte {
	tb.Helper()
	files := make(map[string][]byte, count)
	for i := range count {
		files[fmt.Sprintf("image%d.png", i)] = candidatePNGReuseEncode(tb, apngTestImage(side+i, side-i, i))
	}
	return files
}

func TestCandidatePNGReuseCompatibility(t *testing.T) {
	palette := color.Palette{color.NRGBA{}, color.NRGBA{R: 180, A: 255}, color.NRGBA{B: 90, A: 80}}
	gray := image.NewGray(image.Rect(0, 0, 31, 11))
	gray16 := image.NewGray16(image.Rect(0, 0, 8, 19))
	rgba64 := image.NewNRGBA64(image.Rect(0, 0, 23, 7))
	rgba64.SetNRGBA64(0, 0, color.NRGBA64{R: 32000, B: 12000, A: 30000})
	paletted := image.NewPaletted(image.Rect(0, 0, 17, 29), palette)
	paletted.SetColorIndex(1, 1, 1)
	opaque := image.NewRGBA(image.Rect(0, 0, 19, 13))
	for i := 3; i < len(opaque.Pix); i += 4 {
		opaque.Pix[i] = 255
	}
	images := []image.Image{apngTestImage(12, 10, 1), gray, paletted, gray16, rgba64, opaque}
	for i := range images {
		files := map[string][]byte{}
		for j := range maxCandidateFiles {
			data := candidatePNGReuseEncode(t, images[(i+j)%len(images)])
			files[fmt.Sprintf("image%d.png", j)] = pngWithChunk(data, "tEXt", []byte("Comment\x00synthetic-marker"))
		}
		if i%2 == 0 {
			files["image2.png"] = candidateAPNGFixture(t)
		}
		if i%3 == 0 {
			delete(files, "image1.png")
			files["image1.jpg"] = candidateJPEG(t, 9, 21)
		}
		saved := map[string][]byte{}
		for path, data := range files {
			saved[path] = bytes.Clone(data)
		}
		want, wantCode := previousSanitizeCandidatePNGImages(files, "image0.png")
		got, code := sanitizeCandidateImages(files, "image0.png")
		if code != "" || code != wantCode || !maps.EqualFunc(got, want, bytes.Equal) {
			t.Fatalf("颜色类型、尺寸或混合图片清理结果改变：组合=%d，错误=%q，原错误=%q", i, code, wantCode)
		}
		if !maps.EqualFunc(files, saved, bytes.Equal) {
			t.Fatal("清理修改了上传数据")
		}
	}
}

func TestCandidatePNGReuseMemoryBudget(t *testing.T) {
	files := candidatePNGReuseFixture(t, maxCandidateFiles, 16)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, code := sanitizeCandidateImages(files, "image0.png"); code != "" {
				b.Fatal(code)
			}
		}
	})
	t.Logf("%s；%s", result, result.MemString())
	// 允许完整解码及一个压缩工作区，拒绝三张小 PNG 分别分配压缩器的回归。
	if allocated := result.AllocedBytesPerOp(); allocated > 2<<20 {
		t.Fatalf("三张小 PNG 清理分配 %d 字节，超过 2 MiB 预算", allocated)
	}
}

func BenchmarkCandidatePNGReuse(b *testing.B) {
	for _, tc := range []struct {
		name        string
		count, side int
	}{
		{"single-small", 1, 16}, {"triple-small", 3, 16}, {"triple-medium", 3, 192},
	} {
		files := candidatePNGReuseFixture(b, tc.count, tc.side)
		for _, implementation := range []struct {
			name string
			run  func(map[string][]byte, string) (map[string][]byte, string)
		}{
			{"previous", previousSanitizeCandidatePNGImages}, {"current", sanitizeCandidateImages},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, code := implementation.run(files, "image0.png"); code != "" {
						b.Fatal(code)
					}
				}
			})
		}
	}
}
