package httpjson_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

func TestWriteResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		body  string
	}{
		{"对象与转义", map[string]any{"name": "合成", "html": "<>&"}, `{"html":"\u003c\u003e\u0026","name":"合成"}` + "\n"},
		{"空值", nil, "null\n"},
		{"空列表", []string{}, "[]\n"},
		{"编码失败保留已提交状态", make(chan int), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "private")
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpjson.Write(w, http.StatusCreated, tc.value)
			response := w.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusCreated || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || w.Body.String() != tc.body {
				t.Fatalf("响应发生变化：状态=%d，头=%v，正文=%q", response.StatusCode, response.Header, w.Body.String())
			}
			if response.Header.Get("Cache-Control") != "private" || response.Header.Get("WWW-Authenticate") != "Bearer" || response.Header.Get("Referrer-Policy") != "" {
				t.Fatalf("调用方的响应策略发生变化：%v", response.Header)
			}
		})
	}
}

func TestWriteErrorResponse(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Retry-After", "60")
	httpjson.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded")
	response := w.Result()
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Retry-After") != "60" {
		t.Fatalf("错误响应发生变化：%d %v", response.StatusCode, response.Header)
	}
	if body := w.Body.String(); body != `{"error":{"code":"rate_limit_exceeded","message":"rate_limit_exceeded"}}`+"\n" {
		t.Fatalf("错误封装发生变化：%q", body)
	}
}
