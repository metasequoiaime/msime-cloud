package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNiuTransTransportFailureDoesNotLogSignedURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "synthetic-private-app", apiKey: "synthetic-key"}
	logs := captureLogs(t)
	w := call(s, http.MethodGet, "/v1/niutrans/documents/synthetic-private-file", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, private := range []string{"authStr=", "synthetic-private-app", "synthetic-private-file"} {
		if strings.Contains(logs.String(), private) || strings.Contains(w.Body.String(), private) {
			t.Fatalf("上游错误泄露了 %q", private)
		}
	}
	if !strings.Contains(logs.String(), "upstream failed") || !strings.Contains(logs.String(), "/v1/niutrans/documents/{file_no}") {
		t.Fatalf("没有记录失败原因和路由模式：%s", logs.String())
	}
}

func TestUpstreamTimeoutLogDoesNotIncludeURL(t *testing.T) {
	logs := captureLogs(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/cloud/candidates", nil)
	r.Pattern = "GET /v1/cloud/candidates"
	w := httptest.NewRecorder()
	err := fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: "https://example.test/?api_key=synthetic-private-key", Err: context.DeadlineExceeded})
	upstreamError(w, r, err)
	if w.Code != http.StatusGatewayTimeout || strings.Contains(logs.String(), "synthetic-private-key") || !strings.Contains(logs.String(), "upstream timed out") {
		t.Fatalf("status=%d logs=%s", w.Code, logs.String())
	}
}

func TestChatStreamFallbackDoesNotLogUpstreamURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input chatRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input.Model == "server-model" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contentFrame("备用")+"data: [DONE]\n\n")
	})
	s.config.Chat.URL += "?api_key=synthetic-private-key"
	s.config.Chat.FallbackModel = "fallback-model"
	logs := captureLogs(t)
	w := call(s, http.MethodPost, "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"test"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "备用") || !strings.Contains(logs.String(), "trying the fallback model") {
		t.Fatalf("status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
	}
	if strings.Contains(logs.String(), "synthetic-private-key") {
		t.Fatal("备用模型日志泄露了上游查询串")
	}
}
