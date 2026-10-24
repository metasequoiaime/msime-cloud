package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowedOriginPorts(t *testing.T) {
	t.Setenv("TEST_ALLOWED_ORIGIN_TOKEN", testToken)
	for _, tc := range []struct {
		name, configured, browser string
	}{
		{"无端口", "https://site.example.test", "https://site.example.test"},
		{"默认端口", "https://site.example.test:443", "https://site.example.test"},
		{"默认端口前导零", "https://site.example.test:00443", "https://site.example.test"},
		{"主机大小写", "https://SITE.Example.Test:443", "https://site.example.test"},
		{"非默认端口", "https://site.example.test:8443", "https://site.example.test:8443"},
		{"非默认端口前导零", "https://site.example.test:08443", "https://site.example.test:8443"},
		{"HTTPS 的 80 端口", "https://site.example.test:80", "https://site.example.test:80"},
		{"空端口", "https://site.example.test:", "https://site.example.test"},
		{"IPv6 默认端口", "https://[::1]:443", "https://[::1]"},
		{"IPv6 非默认端口", "https://[::1]:08443", "https://[::1]:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Config{Clients: []Client{{ID: "origin-test", TokenEnv: "TEST_ALLOWED_ORIGIN_TOKEN", RequestsPerMinute: 100}}, AllowedOrigins: []string{tc.configured}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			for _, method := range []string{http.MethodGet, http.MethodOptions} {
				r := httptest.NewRequest(method, "https://api.example.test/v1/capabilities", nil)
				r.Header.Set("Authorization", "Bearer "+testToken)
				r.Header.Set("Origin", tc.browser)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				want := http.StatusOK
				if method == http.MethodOptions {
					want = http.StatusNoContent
				}
				if w.Code != want || w.Header().Get("Access-Control-Allow-Origin") != tc.browser {
					t.Errorf("%s 来源 %q，配置 %q：响应=%d，允许来源=%q，期望=%d %q", method, tc.browser, tc.configured, w.Code, w.Header().Get("Access-Control-Allow-Origin"), want, tc.browser)
				}
			}
			// 只规范化配置；请求仍须精确匹配协议、主机和端口。
			for _, denied := range []string{"https://other.example.test", "http://site.example.test", "https://site.example.test:9443", tc.browser + "/path", tc.browser + ":443"} {
				r := httptest.NewRequest(http.MethodOptions, "https://api.example.test/v1/capabilities", nil)
				r.Header.Set("Origin", denied)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Errorf("意外放行来源 %q：%d", denied, w.Code)
				}
			}
		})
	}
}

func TestAllowedOriginRejectsNonOriginURLs(t *testing.T) {
	t.Setenv("TEST_ALLOWED_ORIGIN_TOKEN", testToken)
	for _, origin := range []string{
		"https://site.example.test?", "https://site.example.test#",
		"https://site.example.test:65536", "https://:443",
		"https://site.example.test/path", "https://user@site.example.test",
	} {
		t.Run(origin, func(t *testing.T) {
			c := Config{Clients: []Client{{ID: "origin-test", TokenEnv: "TEST_ALLOWED_ORIGIN_TOKEN", RequestsPerMinute: 100}}, AllowedOrigins: []string{origin}}
			if err := c.Validate(); err == nil {
				t.Fatalf("意外接受无效来源 %q", origin)
			}
		})
	}
}
