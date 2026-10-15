package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func TestDecodeJSONResponseContract(t *testing.T) {
	for _, tc := range []struct {
		contentType, body, code string
		status                  int
	}{
		{"text/plain", `{}`, "json_required", 415},
		{"application/json", `{} {}`, "invalid_json", 400},
		{"application/json", `{"text":"` + strings.Repeat("x", contract.JsonBodyBytes) + `"}`, "invalid_json", 400},
	} {
		t.Run(tc.code+tc.contentType+tc.body[:2], func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			var value struct {
				Text string `json:"text"`
			}
			if decode(w, r, &value) || w.Code != tc.status || w.Body.String() != `{"error":{"code":"`+tc.code+`","message":"`+tc.code+`"}}`+"\n" {
				t.Fatalf("错误响应不符：%d %q", w.Code, w.Body.String())
			}
			if w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "" || w.Header().Get("Referrer-Policy") != "" {
				t.Fatalf("服务端错误响应头发生变化：%v", w.Header())
			}
		})
	}

	body := `{"text":"` + strings.Repeat("x", 16384) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	var value struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &value) || len(value.Text) != 16384 || w.Body.Len() != 0 {
		t.Fatalf("服务端默认上限应继续大于账户上限：%d %q", w.Code, w.Body.String())
	}
}

func TestDecodeSizedUsesCallerLimit(t *testing.T) {
	const body = `{"text":"synthetic"}`
	for _, limit := range []int64{int64(len(body)), int64(len(body) - 1)} {
		r := httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var value struct {
			Text string `json:"text"`
		}
		if ok := decodeSized(w, r, &value, limit); ok != (limit == int64(len(body))) {
			t.Fatalf("自定义字节上限 %d 未保留：%v %d", limit, ok, w.Code)
		}
	}
}
