package account

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONResponsePrivacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		send   func(http.ResponseWriter)
		status int
		body   string
	}{
		{"成功", func(w http.ResponseWriter) { write(w, 201, map[string]string{"name": "合成"}) }, 201, `{"name":"合成"}` + "\n"},
		{"错误", func(w http.ResponseWriter) { writeError(w, 401, "user_session_required") }, 401, `{"error":{"code":"user_session_required","message":"user_session_required"}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Header().Set("Cache-Control", "public")
			w.Header().Set("Referrer-Policy", "origin")
			w.Header().Set("WWW-Authenticate", "Bearer")
			tc.send(w)
			response := w.Result()
			defer response.Body.Close()
			if response.StatusCode != tc.status || w.Body.String() != tc.body {
				t.Fatalf("账户响应发生变化：%d %q", response.StatusCode, w.Body.String())
			}
			for key, value := range map[string]string{"Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "WWW-Authenticate": "Bearer"} {
				if response.Header.Get(key) != value {
					t.Fatalf("响应头 %s=%q，应为 %q", key, response.Header.Get(key), value)
				}
			}
		})
	}
}
