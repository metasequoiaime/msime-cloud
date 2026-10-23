package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

type skinArtworkReadBody struct {
	io.Reader
	closed bool
}

func (b *skinArtworkReadBody) Close() error { b.closed = true; return nil }

func TestSkinArtworkResponseReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		code   string
		calls  int64
	}{
		{"超时", context.DeadlineExceeded, 504, "upstream_timeout", 1},
		{"网络超时", fmt.Errorf("synthetic-private-network-details: %w", os.ErrDeadlineExceeded), 504, "upstream_timeout", 1},
		{"取消", context.Canceled, 502, "upstream_failure", 0},
		{"截断", io.ErrUnexpectedEOF, 502, "upstream_failure", 1},
		{"私有错误", errors.New("synthetic-private-read-details"), 502, "upstream_failure", 1},
	} {
		for _, mode := range []string{"同步", "异步"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("不应访问真实上游") })
				t.Cleanup(s.Close)
				s.config.Images = s.config.Chat.Endpoint
				s.config.Admin.Enabled = true
				body := &skinArtworkReadBody{Reader: io.MultiReader(strings.NewReader(`{"data":[{"b64_json":"synthetic-private-body"}]}`), iotest.ErrReader(tc.cause))}
				s.client.Transport = qqTestTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
				})
				logs := captureLogs(t)
				var w *httptest.ResponseRecorder
				if mode == "同步" {
					w = call(s, http.MethodPost, "/v1/skins/generate", `{"prompt":"合成插画"}`)
					if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
						t.Errorf("读取失败分类错误：status=%d，期望 status=%d、code=%s", w.Code, tc.status, tc.code)
					}
				} else {
					path := createArtworkJob(t, s)
					s.skinWorkers.Wait()
					w = call(s, http.MethodGet, path, "")
					var result struct {
						State   string          `json:"state"`
						Reason  string          `json:"reason"`
						Artwork json.RawMessage `json:"artwork"`
					}
					if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.State != "failed" || result.Reason != tc.code || string(result.Artwork) != "null" {
						t.Errorf("任务读取失败未保留原因或丢弃部分插画：status=%d，期望 reason=%s", w.Code, tc.code)
					}
				}
				if !body.closed {
					t.Error("读取失败后未关闭上游响应体")
				}
				if strings.Contains(w.Body.String()+logs.String(), "synthetic-private-") {
					t.Error("响应或日志泄露上游读取错误或部分正文")
				}
				wantLog := "upstream failed"
				if tc.status == http.StatusGatewayTimeout {
					wantLog = "upstream timed out"
				}
				if !strings.Contains(logs.String(), wantLog) {
					t.Error("读取失败未记录标准上游错误")
				}
				if counts := pendingCalls(&s.metrics, "images"); counts.calls != tc.calls || counts.errors != tc.calls {
					t.Errorf("读取失败的指标错误：%+v", counts)
				}
			})
		}
	}
}

func TestSkinArtworkOversizedResponseRemainsInvalid(t *testing.T) {
	s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("不应访问真实上游") })
	t.Cleanup(s.Close)
	s.config.Images = s.config.Chat.Endpoint
	s.config.Admin.Enabled = true
	reader := strings.NewReader(strings.Repeat(" ", 12*1024*1024+17))
	body := &skinArtworkReadBody{Reader: reader}
	s.client.Transport = qqTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})
	w := call(s, http.MethodPost, "/v1/skins/generate", `{"prompt":"合成插画"}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"invalid_skin_artwork"`) {
		t.Fatalf("超大响应的错误分类发生变化：status=%d", w.Code)
	}
	if !body.closed || reader.Len() != 16 {
		t.Errorf("响应体未关闭或读取超过上限：closed=%v，剩余字节=%d", body.closed, reader.Len())
	}
	if counts := pendingCalls(&s.metrics, "images"); counts.calls != 1 || counts.errors != 1 {
		t.Errorf("超大响应的指标错误：%+v", counts)
	}
}
