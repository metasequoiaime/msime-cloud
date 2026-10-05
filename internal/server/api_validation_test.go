package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestEnabledAPIInvalidRequests(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached upstream") })
	for _, tc := range []struct{ name, method, path, body, code string }{
		{"chat empty messages", "POST", "/v1/chat/completions", `{"messages":[]}`, "invalid_chat_request"},
		{"chat stream empty messages", "POST", "/v1/chat/completions", `{"messages":[],"stream":true}`, "invalid_chat_request"},
		{"chat invalid role", "POST", "/v1/chat/completions", `{"messages":[{"role":"tool","content":"test"}]}`, "invalid_message"},
		{"chat unknown field", "POST", "/v1/chat/completions", `{"messages":[],"unexpected":true}`, "invalid_json"},
		{"translation blank text", "POST", "/v1/translate", `{"text":" ","source_lang":"AUTO","target_lang":"EN"}`, "invalid_translation_request"},
		{"translation bad language", "POST", "/v1/translate", `{"text":"test","source_lang":"AUTO","target_lang":"en?"}`, "invalid_translation_request"},
		{"cloud blank text", "GET", "/v1/cloud/candidates?text=", ``, "invalid_cloud_request"},
		{"cloud bad limit", "GET", "/v1/cloud/candidates?text=nihao&limit=0", ``, "invalid_cloud_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := call(s, tc.method, tc.path, tc.body)
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
	if w := call(s, "POST", "/v1/audio/transcriptions", `{}`); w.Code != 415 {
		t.Fatalf("transcription accepted non-multipart: %d", w.Code)
	}
}
func TestEnabledAPIRejectsMalformedProviderResults(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"messages":[{"role":"user","content":"test"}]}`},
		{"/v1/translate", `{"text":"test","source_lang":"AUTO","target_lang":"EN"}`},
	} {
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"unexpected":"synthetic-details"}`)
		})
		w := call(s, "POST", tc.path, tc.body)
		if w.Code != 502 || strings.Contains(w.Body.String(), "synthetic-details") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}

func TestAllInputOperationsRejectMalformedRequests(t *testing.T) {
	for _, operation := range []string{"romaji", "japanese", "convert", "annotate", "unicode", "datetime", "english", "gloss", "emoji", "kaomoji", "jianpin", "candidates", "segmentation", "quick", "helpcode"} {
		t.Run(operation, func(t *testing.T) {
			s := fixture(t, nil)
			for _, body := range []string{`{`, `{} {}`, `{"text":"x","unknown":true}`, `{"text":""}`, `{"text":"x","limit":-1}`, `{"text":"x","limit":201}`, `{"text":"\u0000"}`} {
				w := call(s, "POST", "/v1/input/"+operation, body)
				if w.Code != 400 {
					t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
				}
			}
		})
	}
}
