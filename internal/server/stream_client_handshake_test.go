package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func newStreamingHandshakeRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, contract.StreamingTranscriptionPath, nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

func TestStreamingValidatesClientHandshakeBeforeDial(t *testing.T) {
	const marker = "synthetic-private-client-header"
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		valid  bool
	}{
		{name: "缺少 Upgrade", change: func(r *http.Request) { r.Header.Del("Upgrade") }},
		{name: "错误 Upgrade", change: func(r *http.Request) { r.Header.Set("Upgrade", marker) }},
		{name: "缺少 Connection", change: func(r *http.Request) { r.Header.Del("Connection") }},
		{name: "错误 Connection", change: func(r *http.Request) { r.Header.Set("Connection", marker) }},
		{name: "Connection 子串", change: func(r *http.Request) { r.Header.Set("Connection", "not-upgrade") }},
		{name: "Upgrade 子串", change: func(r *http.Request) { r.Header.Set("Upgrade", "not-websocket") }},
		{name: "缺少版本", change: func(r *http.Request) { r.Header.Del("Sec-WebSocket-Version") }},
		{name: "错误版本", change: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", marker) }},
		{name: "缺少 Key", change: func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }},
		{name: "空 Key", change: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "") }},
		{name: "多个 Key", change: func(r *http.Request) { r.Header.Add("Sec-WebSocket-Key", r.Header.Get("Sec-WebSocket-Key")) }},
		{name: "无效 Base64", change: func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", marker) }},
		{name: "Key 长度错误", change: func(r *http.Request) {
			r.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte(marker)))
		}},
		{name: "HTTP 1.0", change: func(r *http.Request) { r.Proto, r.ProtoMinor = "HTTP/1.0", 0 }},
		{name: "HEAD", change: func(r *http.Request) { r.Method = http.MethodHead }},
		{name: "标准握手", change: func(*http.Request) {}, valid: true},
		{name: "大小写与逗号 token", change: func(r *http.Request) {
			r.Header.Set("Connection", "keep-alive, uPgRaDe")
			r.Header.Set("Upgrade", "other, WebSocket")
		}, valid: true},
		{name: "多行 token", change: func(r *http.Request) {
			r.Header.Set("Connection", "keep-alive")
			r.Header.Add("Connection", " Upgrade ")
			r.Header.Set("Upgrade", "other")
			r.Header.Add("Upgrade", " WebSocket ")
		}, valid: true},
		{name: "Key 首尾空白", change: func(r *http.Request) {
			r.Header.Set("Sec-WebSocket-Key", " \t"+r.Header.Get("Sec-WebSocket-Key")+"\t ")
		}, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			s := fixture(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			t.Cleanup(s.Close)
			s.config.Admin.Enabled = true
			s.config.Streaming = StreamingEndpoint{URL: strings.Replace(s.config.Cloud.URL, "https:", "wss:", 1), token: "synthetic-provider-key", ResourceID: "synthetic-resource", MaxSeconds: 5}
			logs := captureLogs(t)
			r := newStreamingHandshakeRequest()
			tc.change(r)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			wantStatus, wantCode, wantCalls := http.StatusBadRequest, "websocket_required", int64(0)
			if tc.valid {
				wantStatus, wantCode, wantCalls = http.StatusBadGateway, "upstream_failure", 1
			}
			if w.Code != wantStatus || !strings.Contains(w.Body.String(), wantCode) || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Errorf("握手响应错误：status=%d，期望 status=%d、code=%s", w.Code, wantStatus, wantCode)
			}
			if got := calls.Load(); got != wantCalls {
				t.Errorf("上游调用次数=%d，期望 %d", got, wantCalls)
			}
			if counts := pendingCalls(&s.metrics, "streaming"); counts.calls != wantCalls || counts.errors != wantCalls {
				t.Errorf("握手指标错误：%+v，期望调用与错误次数均为 %d", counts, wantCalls)
			}
			if strings.Contains(w.Body.String(), marker) || strings.Contains(logs.String(), marker) {
				t.Error("握手校验泄露了客户端请求头原文")
			}
		})
	}
}
