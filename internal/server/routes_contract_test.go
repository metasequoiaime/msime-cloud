package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestEveryPublishedRouteSecurityContract(t *testing.T) {
	raw, err := documentation.ReadFile("swagger/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	count := 0
	for path, operations := range spec.Paths {
		path = strings.NewReplacer("{id}", "missing", "{kind}", "pinyin", "{resource}", "missing.css", "{job}", "missing").Replace(path)
		for method := range operations {
			if method != "get" && method != "post" && method != "put" && method != "patch" && method != "delete" {
				continue
			}
			count++
			t.Run(method+" "+path, func(t *testing.T) {
				s := fixture(t, nil)
				for _, token := range []string{"", "not-a-valid-token"} {
					r := httptest.NewRequest(strings.ToUpper(method), path, strings.NewReader(`{}`))
					r.Header.Set("Authorization", "Bearer "+token)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					expected := 401
					if path == "/healthz" || method == "get" && path == wordSubmissionsPath {
						// The word form settings are anonymous and answer enabled:false when unconfigured.
						expected = 200
					} else if account.IsPath(path) || path == noticesPath {
						// The notices feed needs no credentials and answers 503 without the account service.
						expected = 503
					}
					if w.Code != expected {
						t.Fatalf("authentication: %d want %d: %s", w.Code, expected, w.Body.String())
					}
					if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
						t.Fatal("missing security headers")
					}
				}
				// Every published path must reject cross-origin access before dispatch.
				r := httptest.NewRequest(strings.ToUpper(method), path, nil)
				r.Header.Set("Origin", "https://untrusted.example")
				r.Header.Set("Authorization", "Bearer "+testToken)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if method == "post" && path == account.AppleCallbackPath {
					// Apple 的 form_post 带它自己的 Origin：只有这条路由豁免 Origin 检查，而且不回 CORS 头。
					if w.Code == 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
						t.Fatal("apple callback must skip the origin gate without CORS", w.Code, w.Header())
					}
				} else if w.Code != 403 || !strings.Contains(w.Body.String(), "origin_denied") {
					t.Fatal("cross-origin request accepted", w.Code)
				}
				r = httptest.NewRequest("TRACE", path, nil)
				r.Header.Set("Authorization", "Bearer "+testToken)
				w = httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != 405 || w.Header().Get("Allow") == "" {
					t.Fatal("route not registered or wrong method permitted", w.Code, w.Body.String())
				}
				r = httptest.NewRequest("OPTIONS", "http://example.com"+path, nil)
				r.Header.Set("Origin", "http://example.com")
				w = httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "http://example.com" {
					t.Fatal("same-origin preflight", w.Code)
				}
			})
		}
	}
	if count == 0 {
		t.Fatal("empty API inventory")
	}
	t.Logf("verified security and method contracts for %d published operations", count)
}

// 豁免只针对 POST /v1/auth/apple/callback：Apple 的 Origin 在其他路由上照样被拒，回调的其他方法也不豁免。
func TestAppleOriginAcceptedOnlyOnCallback(t *testing.T) {
	s := fixture(t, nil)
	for _, tc := range []struct{ method, path string }{{"POST", "/v1/auth/apple/web"}, {"POST", "/v1/auth/apple/web/login"}, {"POST", "/v1/auth/login"}, {"GET", "/v1/users/me"}, {"GET", account.AppleCallbackPath}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://appleid.apple.com")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "origin_denied") {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", account.AppleCallbackPath, strings.NewReader("state=x"))
	r.Header.Set("Origin", "https://appleid.apple.com")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code == 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("callback", w.Code, w.Header())
	}
}

func TestEveryAdminRouteAuthenticationAndOrigin(t *testing.T) {
	for _, path := range []string{"overview", "users", "users/example", "downloads", "crashes", "skins", "skins/example", "candidate-skins", "candidate-skins/example", "plugins", "plugins/example", "dictionaries", "dictionaries/example", "replies", "replies/example", "audit", "admins", "actions", "system", "site-settings"} {
		t.Run(path, func(t *testing.T) {
			s := fixture(t, nil)
			s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", token: strings.Repeat("a", 40)}
			method := "GET"
			if path == "actions" {
				method = "POST"
			}
			for _, token := range []string{"", testToken, "wrong"} {
				r := httptest.NewRequest(method, "https://admin.msime.app/api/"+path, strings.NewReader(`{}`))
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("admin gate bypassed", w.Code, w.Body.String())
				}
			}
			r := httptest.NewRequest(method, "https://admin.msime.app/api/"+path, strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 40))
			r.Header.Set("Origin", "https://evil.example")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal("admin origin gate bypassed", w.Code)
			}
		})
	}
}

// The inventory is deliberately explicit: a newly documented API must name its
// behavioral regression tests instead of passing only generic auth checks.
func TestAPICoverageInventory(t *testing.T) {
	var inventory struct {
		Public map[string][]string `json:"public"`
		Admin  map[string][]string `json:"admin_and_docs"`
		Shared []string            `json:"shared"`
	}
	raw, err := os.ReadFile("testdata/api-coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	raw, err = documentation.ReadFile("swagger/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	for path, methods := range spec.Paths {
		for method := range methods {
			if method != "get" && method != "post" && method != "put" && method != "patch" && method != "delete" {
				continue
			}
			key := strings.ToUpper(method) + " " + path
			expected[key] = true
			if len(inventory.Public[key]) == 0 {
				t.Errorf("missing behavioral test mapping for %s", key)
			}
		}
	}
	for key := range inventory.Public {
		if !expected[key] {
			t.Errorf("stale API test mapping %s", key)
		}
	}
	refs := append([]string{}, inventory.Shared...)
	for _, group := range []map[string][]string{inventory.Public, inventory.Admin} {
		for key, tests := range group {
			if len(tests) == 0 {
				t.Errorf("no tests for %s", key)
			}
			refs = append(refs, tests...)
		}
	}
	for _, ref := range refs {
		path, name, ok := strings.Cut(ref, "::")
		if !ok {
			t.Fatalf("invalid test reference %s", ref)
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("../..", path), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name && strings.HasPrefix(name, "Test") {
				found = true
			}
		}
		if !found {
			t.Errorf("behavioral test missing: %s", ref)
		}
	}
}
