package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminGoogleMutationOriginPorts(t *testing.T) {
	t.Setenv("TEST_ADMIN_GOOGLE_ORIGIN_SECRET", "synthetic-secret")
	for _, tc := range []struct {
		name, host, redirect, page, origin string
		allowed                            bool
	}{
		{"HTTPS 无端口", "admin.example.test", "https://admin.example.test", "https://admin.example.test", "https://admin.example.test", true},
		{"HTTPS 默认端口", "admin.example.test", "https://admin.example.test:443", "https://admin.example.test", "https://admin.example.test", true},
		{"HTTPS 默认端口前导零", "admin.example.test", "https://admin.example.test:00443", "https://admin.example.test", "https://admin.example.test", true},
		{"HTTP 本地默认端口", "admin.localhost", "http://admin.localhost:80", "http://admin.localhost", "http://admin.localhost", true},
		{"HTTP 本地默认端口前导零", "admin.localhost", "http://admin.localhost:0080", "http://admin.localhost", "http://admin.localhost", true},
		{"HTTPS 本地默认端口", "admin.localhost", "https://admin.localhost:443", "https://admin.localhost", "https://admin.localhost", true},
		{"HTTPS 非默认端口", "admin.localhost", "https://admin.localhost:8443", "https://admin.localhost:8443", "https://admin.localhost:8443", true},
		{"HTTPS 的 80 端口", "admin.localhost", "https://admin.localhost:80", "https://admin.localhost:80", "https://admin.localhost:80", true},
		{"HTTP 的 443 端口", "admin.localhost", "http://admin.localhost:443", "http://admin.localhost:443", "http://admin.localhost:443", true},
		{"HTTPS 非默认端口前导零", "admin.localhost", "https://admin.localhost:08443", "https://admin.localhost:8443", "https://admin.localhost:8443", true},
		{"主机大小写", "Admin.Example.Test", "https://Admin.Example.Test:443", "https://admin.example.test", "https://admin.example.test", true},
		{"空端口", "admin.example.test", "https://admin.example.test:", "https://admin.example.test", "https://admin.example.test", true},
		{"缺少来源", "admin.example.test", "https://admin.example.test:443", "https://admin.example.test", "", false},
		{"不透明来源", "admin.example.test", "https://admin.example.test:443", "https://admin.example.test", "null", false},
		{"不同主机", "admin.example.test", "https://admin.example.test:443", "https://admin.example.test", "https://other.example.test", false},
		{"不同协议", "admin.localhost", "https://admin.localhost:443", "https://admin.localhost", "http://admin.localhost", false},
		{"不同端口", "admin.localhost", "https://admin.localhost:8443", "https://admin.localhost:8443", "https://admin.localhost", false},
		{"来源含路径", "admin.example.test", "https://admin.example.test:443", "https://admin.example.test", "https://admin.example.test/path", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := AdminGoogleConfig{ClientID: "synthetic-client", SecretEnv: "TEST_ADMIN_GOOGLE_ORIGIN_SECRET", RedirectURI: tc.redirect + adminCallbackPath, AllowedEmails: []string{"owner@example.test"}}
			if err := config.validate(tc.host); err != nil {
				t.Fatal(err)
			}
			token := strings.Repeat("a", 64)
			store := &adminMemoryStore{sessions: map[string]account.AdminIdentity{token: {Subject: "owner", Email: "owner@example.test"}}}
			s := fixture(t, nil)
			s.config.Admin = AdminConfig{Enabled: true, Host: tc.host, Google: config}
			s.adminGoogle = &adminGoogleAuth{}
			s.adminStore = store
			r := httptest.NewRequest(http.MethodPost, tc.page+"/api/auth/logout", nil)
			r.Header.Set("Origin", tc.origin)
			r.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: token})
			w := httptest.NewRecorder()
			if got := s.adminMutationOrigin(w, r); got != tc.allowed {
				t.Fatalf("来源校验=%v，期望=%v，响应=%d %s", got, tc.allowed, w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			if !s.serveAdmin(w, r) {
				t.Fatal("后台未处理退出请求")
			}
			wantCode, wantSessions := http.StatusForbidden, 1
			if tc.allowed {
				wantCode, wantSessions = http.StatusOK, 0
			}
			if w.Code != wantCode || len(store.sessions) != wantSessions {
				t.Fatalf("退出响应=%d，会话数=%d，期望响应=%d，会话数=%d", w.Code, len(store.sessions), wantCode, wantSessions)
			}
		})
	}
}
