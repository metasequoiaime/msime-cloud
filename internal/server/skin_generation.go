package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// Skin artwork is returned inline: clients never download a model-supplied URL.
func (s *Server) generateSkinArtwork(w http.ResponseWriter, r *http.Request) {
	if !enabled(w, s.config.Images) {
		return
	}
	owner, _ := r.Context().Value(skinJobOwnerKey{}).(string)
	if user, ok := strings.CutPrefix(owner, "user:"); ok {
		err := s.accounts.EnsureUserActive(r.Context(), user)
		if errors.Is(err, account.ErrBanned) {
			fail(w, http.StatusForbidden, "account_banned")
			return
		}
		if errors.Is(err, account.ErrInvalid) {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "auth_unavailable")
			return
		}
	}
	var input struct {
		Prompt string `json:"prompt"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if !utf8.ValidString(input.Prompt) || utf8.RuneCountInString(input.Prompt) < 1 || utf8.RuneCountInString(input.Prompt) > 1200 || strings.ContainsRune(input.Prompt, 0) {
		fail(w, 400, "invalid_skin_prompt")
		return
	}
	// Image inference may outlast the normal JSON endpoint write deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(185 * time.Second))
	body, _ := json.Marshal(map[string]any{"model": s.config.Images.Model, "n": 1, "size": "1024x1024", "prompt": "Design an original illustrated mobile keyboard wallpaper, not a screenshot. No text, letters, numbers, keyboard buttons, logos or watermark. Build an immersive scene with distinctive artwork and materials, keep the central typing area quieter and concentrate characters and decoration around the perimeter. User art direction: " + input.Prompt})
	req, err := http.NewRequestWithContext(r.Context(), "POST", s.config.Images.URL, bytes.NewReader(body))
	if err != nil {
		upstreamError(w, r, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.config.Images.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.config.Images.token)
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
	raw, err := io.ReadAll(io.LimitReader(response.Body, 12*1024*1024+1))
	if err == nil && len(raw) > 12*1024*1024 {
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
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 2048 || cfg.Height > 2048 {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	// 尺寸和 JPEG 扫描数通过后再完整解码，避免把只有有效文件头的损坏图片当作成功插画。
	if format == "jpeg" && account.JPEGScanLimitExceeded(data) {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		fail(w, 502, "invalid_skin_artwork")
		return
	}
	respond(w, 200, map[string]any{"b64_json": output.Data[0].Image, "mime_type": "image/" + format, "width": cfg.Width, "height": cfg.Height})
}
