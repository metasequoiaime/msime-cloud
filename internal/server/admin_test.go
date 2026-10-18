package server

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestAdminHostAuthAndIsolation(t *testing.T) {
	s := fixture(t, func(http.ResponseWriter, *http.Request) {})
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", token: strings.Repeat("a", 40), tokenHash: sha256.Sum256([]byte(strings.Repeat("a", 40)))}
	for _, tc := range []struct {
		host, path, token, origin string
		status                    int
	}{
		{"admin.msime.app", "/", "", "", 200},
		{"ADMIN.MSIME.APP:8080", "/users", "", "", 200},
		{"admin.msime.app", "/crashes", "", "", 200},
		{"admin.msime.app", "/api/overview", "", "", 401},
		{"admin.msime.app", "/api/system", strings.Repeat("a", 40), "", 200},
		{"admin.msime.app", "/api/skins/example", "", "", 401},
		{"admin.msime.app", "/api/dictionaries/example", testToken, "", 401},
		{"admin.msime.app", "/api/replies/example", strings.Repeat("a", 40), "https://evil.test", 403},
		{"api.msime.app", "/api/skins/example", testToken, "", 404},
		{"admin.msime.app", "/api/overview", testToken, "", 401},
		{"admin.msime.app", "/api/overview", strings.Repeat("a", 40), "https://evil.test", 403},
		{"api.msime.app", "/api/overview", strings.Repeat("a", 40), "", 401},
		{"api.msime.app", "/api/overview", testToken, "", 404},
		{"admin.msime.app", "/v1/capabilities", testToken, "", 404},
		{"admin.msime.app", "/web/index.html", "", "", 404},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
		if tc.status == 200 && (!strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("Cache-Control") != "no-store") {
			t.Fatal(w.Header())
		}
		if tc.path == "/api/system" && strings.Contains(w.Body.String(), "token") {
			t.Fatal("system endpoint leaked credential metadata", w.Body.String())
		}
	}
	s.config.Admin.Enabled = false
	r := httptest.NewRequest("GET", "http://admin.msime.app/", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("disabled admin is exposed")
	}
}

func TestAdminStaticAssetCachePolicy(t *testing.T) {
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app"}
	request := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(method, "https://admin.msime.app"+path, nil))
		return w
	}
	index := request("GET", "/community")
	// 从当前构建读取资源地址，不绑定会随前端变更的哈希。
	assets := regexp.MustCompile(`(?:src|href)="(/assets/[^" ]+)"`).FindAllStringSubmatch(index.Body.String(), -1)
	if len(assets) < 2 {
		t.Fatal("页面缺少构建资源")
	}
	for _, asset := range assets {
		for _, method := range []string{"GET", "HEAD"} {
			w := request(method, asset[1])
			if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
				t.Fatalf("%s %s: 状态=%d，响应头=%v", method, asset[1], w.Code, w.Header())
			}
			if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Fatal("静态资源缺少安全响应头", w.Header())
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD 不应返回响应正文")
			}
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/community", 200},
		{"GET", "/api/overview", 401},
		{"GET", "/assets/missing.js", 404},
		{"GET", "/assets/", 404},
		{"POST", assets[0][1], 405},
	} {
		w := request(tc.method, tc.path)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%+v: 状态=%d，响应头=%v", tc, w.Code, w.Header())
		}
	}
}

func TestAdminConfigValidation(t *testing.T) {
	t.Setenv("ADMIN_TEST", strings.Repeat("a", 40))
	t.Setenv("CLIENT_TEST", testToken)
	c := AdminConfig{Enabled: true, TokenEnv: "ADMIN_TEST"}
	if err := c.validate(true, []Client{{TokenEnv: "CLIENT_TEST"}}); err != nil || c.Host != "admin.msime.app" {
		t.Fatal(c, err)
	}
	if err := c.validate(false, nil); err == nil {
		t.Fatal("admin requires database")
	}
	c.Host = "https://admin.msime.app"
	if err := c.validate(true, nil); err == nil {
		t.Fatal("URL accepted as host")
	}
	c.Host = "admin.msime.app"
	c.TokenEnv = "CLIENT_TEST"
	if err := c.validate(true, []Client{{TokenEnv: "CLIENT_TEST"}}); err == nil {
		t.Fatal("shared token accepted")
	}
}

func TestAdminAuthEndpointMethodsAndLocalSession(t *testing.T) {
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.localhost", token: strings.Repeat("a", 40), tokenHash: sha256.Sum256([]byte(strings.Repeat("a", 40)))}
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", "session", "", 200}, {"GET", "session", strings.Repeat("a", 40), 200}, {"POST", "session", "", 405},
		{"GET", "logout", "", 405}, {"POST", "logout", "", 200},
		{"GET", "google/start", "", 404}, {"POST", "google/start", "", 405},
		{"GET", "google/callback", "", 404}, {"POST", "google/callback", "", 405}, {"GET", "unknown", "", 404},
	} {
		r := httptest.NewRequest(tc.method, "http://admin.localhost/api/auth/"+tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Origin", "http://admin.localhost")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if tc.path == "session" && tc.method == "GET" {
			authenticated := `"authenticated":false`
			if tc.token != "" {
				authenticated = `"authenticated":true`
			}
			if !strings.Contains(w.Body.String(), authenticated) || !strings.Contains(w.Body.String(), `"version":`) || !strings.Contains(w.Body.String(), `"can_manage_admins":false`) {
				t.Fatal(w.Body.String())
			}
		}
		if tc.path == "logout" && tc.method == "POST" {
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("退出响应不应修改共享的认证 Cookie")
			}
		}
	}
}
