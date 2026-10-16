package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		send   func(http.ResponseWriter)
		status int
		body   string
	}{
		{"成功", func(w http.ResponseWriter) { respond(w, 202, []string{"合成"}) }, 202, `["合成"]` + "\n"},
		{"错误", func(w http.ResponseWriter) { fail(w, 429, "rate_limit_exceeded") }, 429, `{"error":{"code":"rate_limit_exceeded","message":"rate_limit_exceeded"}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "60")
			tc.send(w)
			response := w.Result()
			defer response.Body.Close()
			if response.StatusCode != tc.status || w.Body.String() != tc.body || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("服务端响应发生变化：%d %v %q", response.StatusCode, response.Header, w.Body.String())
			}
			if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Retry-After") != "60" || response.Header.Get("Referrer-Policy") != "" {
				t.Fatalf("调用方的响应策略发生变化：%v", response.Header)
			}
		})
	}
}
