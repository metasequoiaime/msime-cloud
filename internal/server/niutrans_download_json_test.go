package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNiuTransDownloadJSONBusinessResponses(t *testing.T) {
	const marker = "synthetic-private-download-details"
	for _, kind := range []string{"documents", "images", "voice"} {
		for _, tc := range []struct {
			name, contentType, body string
			accepted                bool
		}{
			{"business error", "application/json", `{"code":20040,"msg":"` + marker + `","data":{"apiKey":"synthetic-key"}}`, false},
			{"missing code", "application/json", `{"msg":"` + marker + `"}`, false},
			{"null code", "application/json", `{"code":null,"msg":"` + marker + `"}`, false},
			{"string code", "application/json", `{"code":"200","msg":"` + marker + `"}`, false},
			{"duplicate code", "application/json", `{"code":200,"code":null,"msg":"` + marker + `"}`, false},
			{"invalid JSON", "application/json", `{"code":200,"msg":"` + marker, false},
			{"structured JSON type", "application/problem+json", `{"error":"` + marker + `"}`, false},
			{"case and parameters", "Application/JSON; charset=utf-8", `{"code":20040,"msg":"` + marker + `"}`, false},
			{"successful JSON", "application/json", `{"code":200,"data":"https://download.example.test/synthetic-result.txt"}`, true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				s := fixture(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", tc.contentType)
					_, _ = io.WriteString(w, tc.body)
				})
				s.config.Admin.Enabled = true
				e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
				s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
				logs := captureLogs(t)
				w := call(s, http.MethodGet, "/v1/niutrans/"+kind+"/"+niuTransTestTask(s, kind, "synthetic-task")+"/download", "")
				wantErrors := int64(0)
				if tc.accepted {
					if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != tc.body {
						t.Errorf("成功 JSON 下载被修改：status=%d body=%s", w.Code, w.Body.String())
					}
				} else {
					wantErrors = 1
					var result struct{ Error struct{ Code string } }
					if w.Code != http.StatusBadGateway || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Error.Code != "upstream_failure" {
						t.Errorf("下载业务错误未转换成统一 502：status=%d body=%s", w.Code, w.Body.String())
					}
					if strings.Contains(w.Body.String()+logs.String(), marker) || strings.Contains(w.Body.String()+logs.String(), "synthetic-key") {
						t.Error("下载响应或日志泄露上游错误详情")
					}
				}
				service := map[string]string{"documents": "niutrans_document", "images": "niutrans_image", "voice": "niutrans_voice"}[kind]
				if counts := pendingCalls(&s.metrics, service); counts.calls != 1 || counts.errors != wantErrors {
					t.Errorf("下载业务指标错误：%+v", counts)
				}
			})
		}
	}
}

type niuTransDownloadJSONFailure struct{ err error }

func (r niuTransDownloadJSONFailure) Read([]byte) (int, error) { return 0, r.err }

func TestNiuTransDownloadJSONReadBoundsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		readError error
		status    int
		calls     int64
	}{
		{"exact limit", niuTransResponseBytes, nil, 200, 1},
		{"oversize", niuTransResponseBytes + 32, nil, 502, 1},
		{"truncated", 32, io.ErrUnexpectedEOF, 502, 1},
		{"private read error", 32, errors.New("synthetic-private-read-error"), 502, 1},
		{"timeout", 32, context.DeadlineExceeded, 504, 1},
		{"cancelled", 32, context.Canceled, 502, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected upstream") })
			s.config.Admin.Enabled = true
			data := `{"code":200}` + strings.Repeat(" ", tc.size-len(`{"code":200}`))
			reader := strings.NewReader(data)
			var input io.Reader = reader
			if tc.readError != nil {
				input = io.MultiReader(reader, niuTransDownloadJSONFailure{tc.readError})
			}
			body := &niuTransDownloadBody{Reader: input}
			s.client = &http.Client{Transport: niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: -1, Body: body, Header: http.Header{"Content-Type": {"application/json"}}}, nil
			})}
			logs := captureLogs(t)
			r, call := metered(httptest.NewRequest(http.MethodGet, "/", nil), "niutrans_document", 0)
			w := httptest.NewRecorder()
			s.niuTransDownload(w, r, "https://example.test/download", call)
			if w.Code != tc.status || !body.closed {
				t.Errorf("status=%d closed=%v", w.Code, body.closed)
			}
			if tc.status == 200 && w.Body.String() != data {
				t.Error("合法上限内的 JSON 下载内容改变")
			}
			if tc.status != 200 && strings.Contains(w.Body.String(), `"code":200`) {
				t.Error("校验完成前发送了上游 JSON")
			}
			if read := len(data) - reader.Len(); read > niuTransResponseBytes+1 {
				t.Errorf("读取超过 JSON 控制响应预算：%d", read)
			}
			if strings.Contains(w.Body.String()+logs.String(), "synthetic-private-read-error") {
				t.Error("读取失败的原始错误进入响应或日志")
			}
			wantErrors := tc.calls
			if tc.status == 200 {
				wantErrors = 0
			}
			if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != tc.calls || counts.errors != wantErrors {
				t.Errorf("下载读取失败的指标错误：%+v", counts)
			}
		})
	}
}
