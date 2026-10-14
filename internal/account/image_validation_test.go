package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"testing"
)

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
			_, err := normalizeScreenshot(data)
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
