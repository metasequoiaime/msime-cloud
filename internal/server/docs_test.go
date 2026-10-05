package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocumentationAndBrowserAuthentication(t *testing.T) {
	s := fixture(t, nil)
	s.config.DocsEnabled = true
	for _, path := range []string{"/swagger/", "/swagger/swagger-ui-bundle.js", "/swagger/swagger-ui.css", "/swagger/init.js", "/openapi.json"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("documentation %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/openapi.json", nil))
	var spec struct {
		OpenAPI string         `json:"openapi"`
		Paths   map[string]any `json:"paths"`
	}
	if json.Unmarshal(w.Body.Bytes(), &spec) != nil || spec.OpenAPI != "3.0.3" || len(spec.Paths) != 100 || spec.Paths["/v1/users/me/avatar"] == nil || spec.Paths["/v1/users/me/sessions"] == nil || spec.Paths["/v1/users/me/sessions/{id}"] == nil || spec.Paths["/v1/users/me/phrases"] == nil || spec.Paths["/v1/feedback"] == nil || spec.Paths["/v1/community/reports"] == nil || spec.Paths["/v1/notices"] == nil || spec.Paths["/v1/community/word-submissions"] == nil || spec.Paths["/v1/skins/jobs"] == nil || spec.Paths["/v1/skins/jobs/{job}"] == nil || spec.Paths["/v1/models"] == nil || spec.Paths["/v1/telemetry/events"] == nil || spec.Paths["/v1/community/resources/{id}/save"] == nil || spec.Paths["/v1/community/resources/{id}/apply"] == nil || spec.Paths["/v1/community/stats"] == nil || spec.Paths["/v1/site/download-mirrors"] == nil || spec.Paths["/v1/community/candidate-skins"] == nil || spec.Paths["/v1/community/candidate-skins/{id}"] == nil || spec.Paths["/v1/community/candidate-skins/sync"] == nil || spec.Paths["/v1/community/candidate-skins/{id}/preview"] == nil || spec.Paths["/v1/community/candidate-skins/{id}/download"] == nil || spec.Paths["/v1/community/candidate-skins/{id}/rating"] == nil || spec.Paths["/v1/community/plugins"] == nil || spec.Paths["/v1/community/plugins/{id}"] == nil || spec.Paths["/v1/community/plugins/{id}/download"] == nil || spec.Paths["/v1/community/plugins/{id}/rating"] == nil || spec.Paths["/v1/community/skins/{id}/save"] == nil || spec.Paths["/v1/community/candidate-skins/{id}/save"] == nil || spec.Paths["/v1/community/plugins/{id}/save"] == nil {
		t.Fatal("incomplete OpenAPI")
	}
	for _, tc := range []struct {
		origin, token string
		status        int
	}{
		{"http://example.com", testToken, 200},
		{"https://example.com", testToken, 200},
		{"http://example.com", "wrong", 401},
		{"https://untrusted.example", testToken, 403},
	} {
		r := httptest.NewRequest("GET", "http://example.com/v1/capabilities", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("browser origin/auth status %d want %d", w.Code, tc.status)
		}
	}
	for _, path := range []string{"/swagger-private", "/v1/chat/completions"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		if w.Code != 401 {
			t.Fatal("documentation bypassed API authentication")
		}
	}
}

func TestDocumentationDisabledByDefault(t *testing.T) {
	s := fixture(t, nil)
	for _, enabled := range []bool{false, true} {
		s.config.DocsEnabled = enabled
		for _, path := range []string{"/swagger", "/swagger/", "/swagger/index.html", "/swagger/swagger-ui-bundle.js", "/swagger/swagger-ui.css", "/swagger/init.js", "/swagger/openapi.json", "/openapi.json"} {
			for _, method := range []string{"GET", "HEAD"} {
				for _, token := range []string{"", testToken} {
					r := httptest.NewRequest(method, path, nil)
					if token != "" {
						r.Header.Set("Authorization", "Bearer "+token)
					}
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if !enabled && (w.Code != 404 || w.Header().Get("Location") != "") {
						t.Fatalf("disabled %s %s: %d", method, path, w.Code)
					}
					if enabled && w.Code != 200 && w.Code != 301 && w.Code != 307 {
						t.Fatalf("enabled %s %s: %d", method, path, w.Code)
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("documentation must not be cached")
					}
				}
			}
		}
	}
	s.config.DocsEnabled = false
	if w := call(s, "GET", "/v1/capabilities", ""); w.Code != 200 {
		t.Fatal("disabled docs broke API")
	}
}
