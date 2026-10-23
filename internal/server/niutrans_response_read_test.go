package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func TestNiuTransResponseReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		calls  int64
	}{
		{"超时", context.DeadlineExceeded, 504, 1},
		{"网络超时", fmt.Errorf("synthetic-private-network-details: %w", os.ErrDeadlineExceeded), 504, 1},
		{"取消", context.Canceled, 502, 0},
		{"截断", io.ErrUnexpectedEOF, 502, 1},
		{"私有错误", errors.New("synthetic-private-read-details"), 502, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("不应访问真实上游") })
			s.config.Admin.Enabled = true
			body := &niuTransDownloadBody{Reader: io.MultiReader(strings.NewReader(`{"code":200}`), iotest.ErrReader(tc.cause))}
			s.client.Transport = niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
			})
			logs := captureLogs(t)
			r, call := metered(httptest.NewRequest(http.MethodGet, "/", nil), "niutrans_document", 0)
			data, err := s.niuTransRequest(r, "https://example.test/status", http.MethodGet, "", nil)
			s.settleMeter(call, err == nil)
			if !errors.Is(err, tc.cause) || len(data) != 0 || !body.closed {
				t.Errorf("读取失败后应保留错误链、丢弃部分响应并关闭响应体：err=%v bytes=%d closed=%v", err, len(data), body.closed)
			}
			w := httptest.NewRecorder()
			upstreamError(w, r, err)
			if w.Code != tc.status {
				t.Errorf("status=%d，期望 %d", w.Code, tc.status)
			}
			if strings.Contains(w.Body.String()+logs.String(), "synthetic-private-") {
				t.Error("响应或日志泄露读取错误详情")
			}
			if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != tc.calls || counts.errors != tc.calls {
				t.Errorf("读取失败的指标错误：%+v", counts)
			}
		})
	}
}

func TestNiuTransJSONRoutesReadTimeout(t *testing.T) {
	for _, kind := range []string{"documents", "images", "voice", "resources"} {
		operations := []string{"upload", "status"}
		if kind == "resources" {
			operations = []string{"list"}
		}
		for _, operation := range operations {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("不应访问真实上游") })
				e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
				s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice, s.config.NiuTrans.Resource = e, e, e, e
				s.client.Transport = niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(iotest.ErrReader(context.DeadlineExceeded)), Header: make(http.Header)}, nil
				})
				var w *httptest.ResponseRecorder
				switch operation {
				case "upload":
					var body bytes.Buffer
					mw := multipart.NewWriter(&body)
					_ = mw.WriteField("from", "zh")
					_ = mw.WriteField("to", "en")
					file, err := mw.CreateFormFile("file", "synthetic.txt")
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.WriteString(file, "synthetic")
					_ = mw.Close()
					w = callMultipart(s, http.MethodPost, "/v1/niutrans/"+kind, mw.FormDataContentType(), body.Bytes())
				case "status":
					w = call(s, http.MethodGet, "/v1/niutrans/"+kind+"/"+niuTransTestTask(s, kind, "synthetic-task"), "")
				default:
					w = call(s, http.MethodGet, "/v1/niutrans/resources?action=dictionary", "")
				}
				if w.Code != http.StatusGatewayTimeout || !strings.Contains(w.Body.String(), `"code":"upstream_timeout"`) {
					t.Errorf("读取上游响应超时未返回统一 504：status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
}
