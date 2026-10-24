package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func TestUpstreamJSONReadErrorAtLimit(t *testing.T) {
	captureLogs(t)
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, io.ErrUnexpectedEOF, errors.New("synthetic-private-read-details")} {
		t.Run(cause.Error(), func(t *testing.T) {
			body := &upstreamBufferBody{reader: bytes.NewReader(bytes.Repeat([]byte(" "), contract.UpstreamResponseBytes+1)), cause: cause}
			s := &Server{config: Config{Admin: AdminConfig{Enabled: true}}, client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
			})}}
			r, meter := metered(httptest.NewRequest(http.MethodPost, "/v1/translate", nil), "translation", 6)
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://upstream.example.test", nil)
			if err != nil {
				t.Fatal(err)
			}
			result := "untouched"
			decoded, err := s.doUpstreamJSON(req, &result)
			if decoded || !errors.Is(err, cause) || err.Error() != "upstream transport failed" || result != "untouched" {
				t.Fatalf("超限与读取失败同时发生时丢失原始错误或修改结果：解码=%t，错误=%v，结果=%q", decoded, err, result)
			}
			if body.read != contract.UpstreamResponseBytes+1 || !body.closed || !meter.sent || meter.err != err {
				t.Fatalf("读取、关闭或计量变化：已读=%d，已关闭=%t，计量=%+v", body.read, body.closed, meter)
			}
			w := httptest.NewRecorder()
			upstreamError(w, r, err)
			wantStatus := http.StatusBadGateway
			if errors.Is(cause, context.DeadlineExceeded) {
				wantStatus = http.StatusGatewayTimeout
			}
			if w.Code != wantStatus {
				t.Fatalf("读取错误的状态分类变化：%d，期望 %d", w.Code, wantStatus)
			}
			s.settleMeter(meter, false)
			wantCalls := int64(1)
			if errors.Is(cause, context.Canceled) {
				wantCalls = 0
			}
			if got := pendingCalls(&s.metrics, "translation"); got.calls != wantCalls || got.errors != wantCalls || got.usage != 0 {
				t.Fatalf("读取错误的结算变化：%+v", got)
			}
		})
	}
}
