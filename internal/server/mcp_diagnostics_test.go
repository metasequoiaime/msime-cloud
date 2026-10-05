package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// /mcp/s/{id} 绕过全局 Bearer 检查（设备令牌不能读快照），没有用户数据库时返回 503；跨站浏览器请求照样被 Origin 检查拦下，不带 Origin 的 MCP 客户端不受影响。
func TestDiagnosticsMCPRoute(t *testing.T) {
	s := fixture(t, nil)
	for _, tc := range []struct {
		method, origin, token string
		status                int
	}{
		{"POST", "", "", 503},
		{"POST", "", testToken, 503},
		{"GET", "", "msk_x", 503},
		{"POST", "https://untrusted.example", "msk_x", 403},
		{"PUT", "", "msk_x", 405},
	} {
		r := httptest.NewRequest(tc.method, "/mcp/s/0123456789abcdef01234567", strings.NewReader(`{}`))
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
}
