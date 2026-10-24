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
		{"中文域名", "https://例子.example.test", "https://xn--fsqu00a.example.test"},
		{"国际域名大小写", "https://BÜCHER.example.test:00443", "https://xn--bcher-kva.example.test"},
		{"国际域名非默认端口", "https://例子.example.test:08443", "https://xn--fsqu00a.example.test:8443"},
		{"非过渡映射", "https://faß.example.test", "https://xn--fa-hia.example.test"},
		{"组合字符", "https://fa\u0308ss.example.test", "https://xn--fss-qla.example.test"},
		{"全角字符", "https://ＦＯＯ.example.test", "https://foo.example.test"},
		{"域名全角句点", "https://例子。example.test", "https://xn--fsqu00a.example.test"},
		{"符号域名", "https://☕.example.test", "https://xn--53h.example.test"},
		{"IPv6 完整写法", "https://[2001:0DB8:0000:0000:0000:0000:0000:0001]", "https://[2001:db8::1]"},
		{"IPv6 完整写法非默认端口", "https://[2001:0db8:0:0:0:0:0:1]:08443", "https://[2001:db8::1]:8443"},
		{"IPv4 映射 IPv6", "https://[::ffff:192.0.2.1]", "https://[::ffff:c000:201]"},
		{"IPv6 单个零保留", "https://[1:0:2:3:4:5:6:7]", "https://[1:0:2:3:4:5:6:7]"},
		{"ASCII 下划线", "https://_service.example.test", "https://_service.example.test"},
		{"ASCII 连续横线", "https://a---b.example.test", "https://a---b.example.test"},
		{"ASCII Punycode 兼容", "https://XN--8I7CAA.example.test", "https://xn--8i7caa.example.test"},
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
			deniedOrigins := []string{"https://other.example.test", "http://site.example.test", "https://site.example.test:9443", tc.browser + "/path", tc.browser + ":443"}
			if tc.configured != tc.browser {
				deniedOrigins = append(deniedOrigins, tc.configured)
			}
			for _, denied := range deniedOrigins {
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
		"https://[not-ip]", "https://[127.0.0.1]", "https://[fe80::1%25en0]",
		"https://\u200d.example.test", "https://example\u00a0.test",
	} {
		t.Run(origin, func(t *testing.T) {
			c := Config{Clients: []Client{{ID: "origin-test", TokenEnv: "TEST_ALLOWED_ORIGIN_TOKEN", RequestsPerMinute: 100}}, AllowedOrigins: []string{origin}}
			if err := c.Validate(); err == nil {
				t.Fatalf("意外接受无效来源 %q", origin)
			}
		})
	}
}
