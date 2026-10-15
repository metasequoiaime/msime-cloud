package account

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadJSONResponseContract(t *testing.T) {
	for _, tc := range []struct {
		contentType, body, code string
		status                  int
	}{
		{"text/plain", `{}`, "json_required", 415},
		{"application/json", `{} {}`, "invalid_json", 400},
		{"application/json", `{"text":"` + strings.Repeat("x", 16384) + `"}`, "invalid_json", 400},
	} {
		t.Run(tc.code+tc.contentType+tc.body[:2], func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			var value struct {
				Text string `json:"text"`
			}
			if read(w, r, &value) || w.Code != tc.status || w.Body.String() != `{"error":{"code":"`+tc.code+`","message":"`+tc.code+`"}}`+"\n" {
				t.Fatalf("错误响应不符：%d %q", w.Code, w.Body.String())
			}
			if w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatalf("账户错误响应头发生变化：%v", w.Header())
			}
		})
	}
}

func TestReadSizedUsesCallerLimit(t *testing.T) {
	const body = `{"text":"synthetic"}`
	for _, limit := range []int64{int64(len(body)), int64(len(body) - 1)} {
		r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var value struct {
			Text string `json:"text"`
		}
		if ok := readSized(w, r, &value, limit); ok != (limit == int64(len(body))) {
			t.Fatalf("自定义字节上限 %d 未保留：%v %d", limit, ok, w.Code)
		}
	}
}
