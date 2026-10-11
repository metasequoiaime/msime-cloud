package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatModelSelectionAndCatalog(t *testing.T) {
	calls := 0
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var v chatRequest
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if v.Model != "everyapi-chat" {
			t.Errorf("model = %q", v.Model)
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("credential isolation")
		}
		respond(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "你好"}}}})
	})
	s.config.Chat.Models = []string{"server-model", "everyapi-chat"}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != 401 {
		t.Fatal("unauthenticated catalog", w.Code)
	}
	w = call(s, "GET", "/v1/models", "")
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Default string `json:"default_model"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil || len(catalog.Data) != 2 || catalog.Default != "server-model" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call(s, "POST", "/v1/chat/completions", `{"model":"everyapi-chat","messages":[{"role":"user","content":"test"}]}`)
	if w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call(s, "POST", "/v1/chat/completions", `{"model":"unapproved-expensive-model","messages":[{"role":"user","content":"test"}]}`)
	if w.Code != 400 || calls != 1 {
		t.Fatal("unapproved model reached upstream")
	}
}
func TestChatModelConfiguration(t *testing.T) {
	for _, models := range [][]string{{""}, {"bad\nmodel"}, {"same", "same"}, make([]string, 33)} {
		c := Config{Chat: ChatEndpoint{Endpoint: Endpoint{Models: models}}}
		if c.Validate() == nil {
			t.Fatal("invalid models accepted", models)
		}
	}
}
