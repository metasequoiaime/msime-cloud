package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"testing"
)

func TestSkinArtworkUsesFixedModelAndReturnsValidatedImage(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24))); err != nil {
		t.Fatal(err)
	}
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Accept")) != 0 || r.ContentLength <= 0 {
			t.Error("图片请求的方法、正文类型、响应内容协商或精确长度变化")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "server-model" || body["n"] != float64(1) || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("invalid image upstream request")
		}
		respond(w, 200, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(encoded.Bytes())}}})
	})
	s.config.Images = s.config.Chat.Endpoint
	w := call(s, "POST", "/v1/skins/generate", `{"prompt":"森林里的小狐狸，绘本风格"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		Width, Height int
		MIME          string `json:"mime_type"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Width != 32 || out.Height != 24 || out.MIME != "image/png" {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{`{"prompt":""}`, `{"prompt":"test","model":"other"}`, `{"prompt":"test","url":"https://example.com"}`} {
		if w = call(s, "POST", "/v1/skins/generate", body); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
func TestSkinArtworkRejectsRemoteURLAndDisabledProvider(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []any{map[string]string{"url": "http://127.0.0.1/private"}}})
	})
	if w := call(s, "POST", "/v1/skins/generate", `{"prompt":"test"}`); w.Code != 503 {
		t.Fatal(w.Code)
	}
	s.config.Images = s.config.Chat.Endpoint
	if w := call(s, "POST", "/v1/skins/generate", `{"prompt":"test"}`); w.Code != 502 {
		t.Fatal(w.Code)
	}
}
