package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestDecodePNGJPEG(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			data := encodedImage(t, format, 8, 4, color.Black)
			truncated := data[:33]
			if format == "jpeg" {
				start := bytes.Index(data, []byte{0xff, 0xda})
				if start < 0 {
					t.Fatal("合成 JPEG 缺少扫描段")
				}
				truncated = data[:start+10]
			}
			if _, _, err := image.DecodeConfig(bytes.NewReader(truncated)); err != nil {
				t.Fatal("截断样本必须保留有效文件头", err)
			}
			for _, check := range []struct {
				name      string
				data      []byte
				side      int
				pixels    int
				wantValid bool
			}{
				{"at limits", data, 8, 32, true},
				{"no pixel limit", data, 8, 0, true},
				{"side exceeded", data, 7, 0, false},
				{"height exceeded", encodedImage(t, format, 4, 8, color.Black), 7, 0, false},
				{"pixels exceeded", data, 8, 31, false},
				{"invalid side limit", data, 0, 0, false},
				{"invalid pixel limit", data, 8, -1, false},
				{"truncated", truncated, 8, 32, false},
			} {
				t.Run(check.name, func(t *testing.T) {
					source, config, gotFormat, ok := DecodePNGJPEG(check.data, check.side, check.pixels)
					if ok != check.wantValid {
						t.Fatalf("有效性：got %v, want %v", ok, check.wantValid)
					}
					if !ok {
						if source != nil || config != (image.Config{}) || gotFormat != "" {
							t.Fatal("拒绝图片时不应返回部分解码结果")
						}
						return
					}
					if source == nil || source.Bounds() != image.Rect(0, 0, 8, 4) || config.Width != 8 || config.Height != 4 || config.ColorModel == nil || gotFormat != format {
						t.Fatalf("解码结果发生变化：config=%v format=%q", config, gotFormat)
					}
					if r, g, b, a := source.At(0, 0).RGBA(); r != 0 || g != 0 || b != 0 || a != 65535 {
						t.Fatal("图片像素发生变化", r, g, b, a)
					}
				})
			}
		})
	}
	for _, data := range [][]byte{nil, []byte("not an image"), encodedImage(t, "gif", 8, 4, color.Black)} {
		if source, config, format, ok := DecodePNGJPEG(data, 8, 32); ok || source != nil || config != (image.Config{}) || format != "" {
			t.Fatal("接受了不支持的图片")
		}
	}
	for _, scans := range []int{32, 33} {
		data := candidateJPEGWithScans(t, candidateJPEG(t, 8, 4), scans)
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Fatal("扫描数样本必须可解码", err)
		}
		if _, _, _, ok := DecodePNGJPEG(data, 8, 32); ok != (scans <= 32) {
			t.Fatalf("JPEG 扫描数 %d 的有效性错误：%v", scans, ok)
		}
	}
}

func TestUploadedImageDimensionLimits(t *testing.T) {
	for _, check := range []struct {
		name string
		side int
		run  func([]byte) error
		want error
	}{
		{"avatar", maxAvatarSourceSide, func(data []byte) error {
			_, err := normalizeAvatar(data)
			return err
		}, errInvalidAvatar},
		{"screenshot", maxFeedbackScreenshotSide, func(data []byte) error {
			_, err := normalizeScreenshot(data, nil)
			return err
		}, errInvalidScreenshot},
	} {
		for _, format := range []string{"png", "jpeg"} {
			for _, size := range [][2]int{{check.side, 1}, {1, check.side}, {check.side + 1, 1}, {1, check.side + 1}} {
				t.Run(fmt.Sprintf("%s/%s/%dx%d", check.name, format, size[0], size[1]), func(t *testing.T) {
					err := check.run(encodedImage(t, format, size[0], size[1], color.Black))
					if size[0] <= check.side && size[1] <= check.side {
						if err != nil {
							t.Fatal("边长恰好达到上限的图片被拒绝", err)
						}
					} else if err != check.want {
						t.Fatalf("超出边长上限：got %v, want %v", err, check.want)
					}
				})
			}
		}
	}
}

func TestUploadedJPEGScanLimit(t *testing.T) {
	jpegData := candidateJPEG(t, 8, 8)
	manifest, files := candidateFixture(t, "scan-limit")
	checks := []struct {
		name string
		run  func([]byte) error
		want error
	}{
		{"avatar", func(data []byte) error {
			_, err := normalizeAvatar(data)
			return err
		}, errInvalidAvatar},
		{"screenshot", func(data []byte) error {
			_, err := normalizeScreenshot(data, nil)
			return err
		}, errInvalidScreenshot},
		{"community photo", func(data []byte) error {
			var design map[string]any
			if err := json.Unmarshal([]byte(communityFixture), &design); err != nil {
				t.Fatal(err)
			}
			design["photo"] = data
			raw, err := json.Marshal(design)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseCommunityDesign(raw)
			return err
		}, ErrInvalid},
		{"candidate skin", func(data []byte) error {
			files["assets/deco.jpg"] = data
			if code := candidatePipeline(manifest, files); code != "" {
				return errors.New(code)
			}
			return nil
		}, errors.New("candidate_skin_image_invalid")},
	}
	for _, scans := range []int{1, 32, 33, 1000} {
		data := candidateJPEGWithScans(t, jpegData, scans)
		// 样本在所有上传入口的字节和尺寸上限内，且能被 Go 解码，拒绝必须来自扫描数限制。
		if len(data) > 512000 {
			t.Fatal("fixture exceeds upload limit", len(data))
		}
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Fatal("decoder refused fixture", err)
		}
		for _, check := range checks {
			t.Run(fmt.Sprintf("%s/%d", check.name, scans), func(t *testing.T) {
				err := check.run(data)
				if scans <= 32 {
					if err != nil {
						t.Fatal("valid scan count refused", err)
					}
				} else if err == nil || err.Error() != check.want.Error() {
					t.Fatalf("got %v, want %v", err, check.want)
				}
			})
		}
	}
}
