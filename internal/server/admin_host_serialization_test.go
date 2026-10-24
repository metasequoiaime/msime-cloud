package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminConfiguredHostSerialization(t *testing.T) {
	const token = "synthetic-admin-token-at-least-32-bytes"
	t.Setenv("TEST_ADMIN_HOST_TOKEN", token)
	for _, tc := range []struct{ name, configured, browser string }{
		{"中文域名", "管理.example.test", "xn--8ey35n.example.test"},
		{"德语域名", "bücher.example.test", "xn--bcher-kva.example.test"},
		{"全角字母和句点", "ＡＤＭＩＮ。example.test", "admin.example.test"},
		{"ASCII 大小写", "ADMIN.example.test", "admin.example.test"},
		{"全部全角句点", "admin。example。test", "admin.example.test"},
		{"Punycode 对照", "xn--bcher-kva.example.test", "xn--bcher-kva.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := AdminConfig{Enabled: true, Host: tc.configured, TokenEnv: "TEST_ADMIN_HOST_TOKEN"}
			if err := c.validate(true, nil); err != nil {
				t.Fatal("后台配置校验失败", err)
			}
			s := fixture(t, nil)
			s.config.Admin = c
			for _, credential := range []string{token, ""} {
				r := httptest.NewRequest(http.MethodGet, "https://"+tc.browser+"/api/auth/session", nil)
				if credential != "" {
					r.Header.Set("Authorization", "Bearer "+credential)
				}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				want := `"authenticated":false`
				if credential != "" {
					want = `"authenticated":true`
				}
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
					t.Errorf("浏览器 Host 未命中后台：状态=%d，响应=%s", w.Code, w.Body.String())
				}
			}
			r := httptest.NewRequest(http.MethodGet, "https://other.example.test/api/auth/session", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			if s.serveAdmin(httptest.NewRecorder(), r) {
				t.Error("其他域名错误进入后台")
			}
		})
	}
}

func TestAdminGoogleConfiguredHostSerialization(t *testing.T) {
	t.Setenv("TEST_ADMIN_HOST_GOOGLE_SECRET", "synthetic-google-secret")
	t.Setenv("TEST_ADMIN_HOST_NO_TOKEN", "")
	for _, tc := range []struct{ name, host, redirect string }{
		{"默认回调", "bücher.example.test", ""},
		{"Unicode 配置与 ASCII 回调", "bücher.example.test", "https://xn--bcher-kva.example.test" + adminCallbackPath},
		{"ASCII 配置与 Unicode 回调", "xn--bcher-kva.example.test", "https://bücher.example.test" + adminCallbackPath},
		{"保留显式回调大小写和端口", "ADMIN.example.test", "https://ADMIN.example.test:0443" + adminCallbackPath},
		{"保留显式 Unicode 回调", "bücher.example.test", "https://bücher.example.test:8443" + adminCallbackPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := AdminConfig{Enabled: true, Host: tc.host, TokenEnv: "TEST_ADMIN_HOST_NO_TOKEN", Google: AdminGoogleConfig{
				ClientID: "synthetic-client", SecretEnv: "TEST_ADMIN_HOST_GOOGLE_SECRET", RedirectURI: tc.redirect, AllowedEmails: []string{"owner@example.test"},
			}}
			if err := c.validate(true, nil); err != nil {
				t.Fatal("等价后台域名的 Google 配置被拒绝", err)
			}
			want := tc.redirect
			if want == "" {
				want = "https://xn--bcher-kva.example.test" + adminCallbackPath
			}
			if c.Google.RedirectURI != want {
				t.Errorf("回调地址=%q，期望=%q", c.Google.RedirectURI, want)
			}
		})
	}
}

func TestAdminConfiguredHostInvalidUnicode(t *testing.T) {
	t.Setenv("TEST_ADMIN_HOST_TOKEN", "synthetic-admin-token-at-least-32-bytes")
	for _, host := range []string{"ad\u200dmin.example.test", "ad\u00a0min.example.test", "admin．example／test", "admin.example.test\u0001", strings.Repeat("ü.", 40) + "example.test"} {
		t.Run(host, func(t *testing.T) {
			c := AdminConfig{Enabled: true, Host: host, TokenEnv: "TEST_ADMIN_HOST_TOKEN"}
			if err := c.validate(true, nil); err == nil {
				t.Error("无法序列化的后台主机被接受")
			}
		})
	}
}

func TestAdminGoogleConfiguredHostRejectsDifferentCallback(t *testing.T) {
	t.Setenv("TEST_ADMIN_HOST_TOKEN", "synthetic-admin-token-at-least-32-bytes")
	t.Setenv("TEST_ADMIN_HOST_GOOGLE_SECRET", "synthetic-google-secret")
	for _, redirect := range []string{
		"https://other.example.test" + adminCallbackPath,
		"https://xn--bcher-kva.example.test.evil.test" + adminCallbackPath,
		"http://xn--bcher-kva.example.test" + adminCallbackPath,
		"https://xn--bcher-kva.example.test/other",
		"https://xn--bcher-kva.example.test" + adminCallbackPath + "?",
		"https://xn--bcher-kva.example.test" + adminCallbackPath + "#",
		"https://user@xn--bcher-kva.example.test" + adminCallbackPath,
	} {
		t.Run(redirect, func(t *testing.T) {
			c := AdminConfig{Enabled: true, Host: "bücher.example.test", TokenEnv: "TEST_ADMIN_HOST_TOKEN", Google: AdminGoogleConfig{
				ClientID: "synthetic-client", SecretEnv: "TEST_ADMIN_HOST_GOOGLE_SECRET", RedirectURI: redirect, AllowedEmails: []string{"owner@example.test"},
			}}
			if err := c.validate(true, nil); err == nil {
				t.Error("不同来源或含额外组成部分的回调被接受")
			}
		})
	}
}
