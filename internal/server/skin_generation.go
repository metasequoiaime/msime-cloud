package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpdeadline"
)

// Skin artwork is returned inline: clients never download a model-supplied URL.
func (s *Server) generateSkinArtwork(w http.ResponseWriter, r *http.Request) {
	if !enabled(w, s.config.Images) {
		return
	}
	if !s.ensureUserOwnerActive(w, r) {
		return
	}
	input, ok := readSkinArtworkInput(w, r)
	if !ok {
		return
	}
	// 图片推理可能超过普通 JSON 接口的写入期限。
	httpdeadline.Extend(w, 0, 185*time.Second)
	req, err := s.config.Images.newJSONBodyRequest(r.Context(), http.MethodPost, map[string]any{"model": s.config.Images.Model, "n": 1, "size": "1024x1024", "prompt": "Design an original illustrated mobile keyboard wallpaper, not a screenshot. No text, letters, numbers, keyboard buttons, logos or watermark. Build an immersive scene with distinctive artwork and materials, keep the central typing area quieter and concentrate characters and decoration around the perimeter. User art direction: " + input.Prompt})
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	client := *s.client
	client.Timeout = 180 * time.Second
	started := time.Now()
	response, err := client.Do(req)
	if err != nil {
		s.observe("images", started, err, 0)
		upstreamError(w, r, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err = errors.New("image generation rejected")
		s.observe("images", started, err, 0)
		upstreamError(w, r, err)
		return
	}
	raw, err := httpclient.ReadResponseBody(response, 12*1024*1024)
	if err != nil {
		err = upstreamTransportError{cause: err}
		s.observe("images", started, err, 0)
		upstreamError(w, r, err)
		return
	}
	if len(raw) > 12*1024*1024 {
		err = errors.New("image generation response too large")
	}
	// The console's images metric covers the upstream exchange; the artwork checks below are this server's own validation.
	s.observe("images", started, err, 0)
	if err != nil {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	var output struct {
		Data []struct {
			Image string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &output) != nil || len(output.Data) != 1 {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	data, err := base64.StdEncoding.DecodeString(output.Data[0].Image)
	if err != nil || len(data) == 0 || len(data) > 8*1024*1024 {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	// 尺寸和 JPEG 扫描数通过后再完整解码，避免把只有有效文件头的损坏图片当作成功插画。
	_, cfg, format, valid := account.DecodePNGJPEG(data, 2048, 0)
	if !valid {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	respond(w, 200, map[string]any{"b64_json": output.Data[0].Image, "mime_type": "image/" + format, "width": cfg.Width, "height": cfg.Height})
}
