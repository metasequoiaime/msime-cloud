package server

import (
	"context"
	"crypto/sha1" // #nosec G505 -- WebSocket 握手协议要求 SHA-1，不用于密码或身份认证。
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamingHandshakeFailuresDoNotLogUpstreamHeaders(t *testing.T) {
	const marker = "synthetic-private-upstream-header"
	for _, header := range []string{"Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
		t.Run(header, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) // #nosec G401 -- 计算协议规定的握手响应，不用于密码或身份认证。
				headers := http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Accept": {base64.StdEncoding.EncodeToString(accept[:])}}
				headers.Set(header, marker)
				if header == "Sec-WebSocket-Extensions" {
					headers.Set(header, "permessage-deflate, "+marker)
				}
				if _, err := fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
					t.Error(err)
					return
				}
				if err := headers.Write(conn); err != nil {
					t.Error(err)
					return
				}
				if _, err := fmt.Fprint(conn, "\r\n"); err != nil {
					t.Error(err)
				}
			})
			t.Cleanup(s.Close)
			s.config.Admin.Enabled = true
			s.config.Streaming = StreamingEndpoint{URL: strings.Replace(s.config.Cloud.URL, "https:", "wss:", 1), token: "synthetic-provider-key", ResourceID: "synthetic-resource", MaxSeconds: 5}
			logs := captureLogs(t)
			r := newStreamingHandshakeRequest()
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream_failure") || !strings.Contains(logs.String(), "upstream failed") {
				t.Fatalf("握手失败未返回并记录标准错误：status=%d", w.Code)
			}
			if strings.Contains(logs.String(), marker) || strings.Contains(w.Body.String(), marker) {
				t.Error("握手失败泄露了上游响应头原文")
			}
			if counts := pendingCalls(&s.metrics, "streaming"); counts.calls != 1 || counts.errors != 1 {
				t.Fatal("握手失败没有计入服务指标", counts)
			}
		})
	}
}

func TestStreamingHandshakeErrorsKeepClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		code   string
		calls  int64
	}{
		{name: "超时", cause: context.DeadlineExceeded, status: http.StatusGatewayTimeout, code: "upstream_timeout", calls: 1},
		{name: "取消", cause: context.Canceled, status: http.StatusBadGateway, code: "upstream_failure", calls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("失败传输不应到达上游") })
			t.Cleanup(s.Close)
			s.config.Admin.Enabled = true
			s.config.Streaming = StreamingEndpoint{URL: "wss://example.test/asr", token: "synthetic-key", ResourceID: "synthetic-resource", MaxSeconds: 5}
			s.client.Transport = qqTestTransport(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("synthetic-private-transport-details: %w", tc.cause)
			})
			logs := captureLogs(t)
			r := newStreamingHandshakeRequest()
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatal("握手错误分类发生变化", w.Code)
			}
			if strings.Contains(logs.String(), "synthetic-private-transport-details") || strings.Contains(w.Body.String(), "synthetic-private-transport-details") {
				t.Fatal("握手错误泄露了传输详情")
			}
			if counts := pendingCalls(&s.metrics, "streaming"); counts.calls != tc.calls || counts.errors != tc.calls {
				t.Fatal("握手错误计量发生变化", counts)
			}
		})
	}
}
