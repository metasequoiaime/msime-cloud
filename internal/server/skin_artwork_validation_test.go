package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strconv"
	"testing"
)

func TestSkinArtworkRejectsTruncatedImages(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			var encoded bytes.Buffer
			source := image.NewRGBA(image.Rect(0, 0, 32, 24))
			var err error
			if format == "png" {
				err = png.Encode(&encoded, source)
			} else {
				err = jpeg.Encode(&encoded, source, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			// 保留有效文件头，移除图像正文，确保旧的尺寸检查仍能通过。
			data := encoded.Bytes()[:33]
			if format == "jpeg" {
				start := bytes.Index(encoded.Bytes(), []byte{0xff, 0xda})
				if start < 0 {
					t.Fatal("合成 JPEG 缺少扫描段")
				}
				data = encoded.Bytes()[:start+10]
			}
			if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
				t.Fatalf("合成文件头无法读取：%v", err)
			}
			if _, _, err := image.Decode(bytes.NewReader(data)); err == nil {
				t.Fatal("合成图片没有损坏")
			}
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(data)}}})
			})
			t.Cleanup(s.Close)
			s.config.Images = s.config.Chat.Endpoint
			w := call(s, "POST", "/v1/skins/generate", `{"prompt":"合成插画"}`)
			if w.Code != http.StatusBadGateway || !bytes.Contains(w.Body.Bytes(), []byte("invalid_skin_artwork")) {
				t.Errorf("损坏图片应被拒绝：status=%d body=%s", w.Code, w.Body.String())
			}
			path := createArtworkJob(t, s)
			s.skinWorkers.Wait()
			w = call(s, "GET", path, "")
			var result struct {
				State   string          `json:"state"`
				Reason  string          `json:"reason"`
				Artwork json.RawMessage `json:"artwork"`
			}
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "failed" || result.Reason != "invalid_skin_artwork" || string(result.Artwork) != "null" {
				t.Fatalf("损坏图片不应成为成功任务：status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSkinArtworkJPEGScanLimit(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24)), nil); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	start, end := bytes.Index(data, []byte{0xff, 0xda}), bytes.LastIndex(data, []byte{0xff, 0xd9})
	if start < 0 || end < start {
		t.Fatal("合成 JPEG 缺少扫描段")
	}
	for _, scans := range []int{1, 32, 33} {
		t.Run(strconv.Itoa(scans), func(t *testing.T) {
			// 重复真正可解码的扫描段，在完整解码前验证耗时边界。
			data := append(append(bytes.Clone(data[:start]), bytes.Repeat(data[start:end], scans)...), 0xff, 0xd9)
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": encoded}}})
			})
			t.Cleanup(s.Close)
			s.config.Images = s.config.Chat.Endpoint
			w := call(s, "POST", "/v1/skins/generate", `{"prompt":"合成插画"}`)
			if scans > 32 {
				if w.Code != http.StatusBadGateway || !bytes.Contains(w.Body.Bytes(), []byte("invalid_skin_artwork")) {
					t.Fatalf("超过扫描数上限应被拒绝：status=%d", w.Code)
				}
				return
			}
			var result struct {
				Image         string `json:"b64_json"`
				MIME          string `json:"mime_type"`
				Width, Height int
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Image != encoded || result.MIME != "image/jpeg" || result.Width != 32 || result.Height != 24 {
				t.Fatalf("合法 JPEG 被拒绝或发生变化：status=%d", w.Code)
			}
		})
	}
}
