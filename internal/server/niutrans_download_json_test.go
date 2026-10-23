package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type niuTransJSONDownloadWriter struct {
	*httptest.ResponseRecorder
	limit int
	err   error
}

func (w *niuTransJSONDownloadWriter) Write(p []byte) (int, error) {
	if w.limit >= 0 && len(p) > w.limit {
		p = p[:w.limit]
	}
	n, _ := w.ResponseRecorder.Write(p)
	return n, w.err
}

func TestNiuTransDownloadJSONWriteOutcomes(t *testing.T) {
	privateFailure := errors.New("synthetic-private-write-details")
	for _, tc := range []struct {
		name      string
		size      int
		limit     int
		cause     error
		wantBytes int
		wantAbort bool
	}{
		{name: "小响应", size: 32, limit: -1, wantBytes: 32},
		{name: "跨复制缓冲区", size: 64 << 10, limit: -1, wantBytes: 64 << 10},
		{name: "恰好JSON上限", size: niuTransResponseBytes, limit: -1, wantBytes: niuTransResponseBytes},
		{name: "短写", size: 64 << 10, limit: 7, wantBytes: 7, wantAbort: true},
		{name: "零写", size: 64 << 10, limit: 0, wantAbort: true},
		{name: "部分写出后失败", size: 64 << 10, limit: 7, cause: privateFailure, wantBytes: 7, wantAbort: true},
		{name: "完整写出但返回错误", size: 32, limit: -1, cause: privateFailure, wantBytes: 32, wantAbort: true},
		{name: "取消", size: 32, limit: 0, cause: context.Canceled, wantAbort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := niuTransBufferJSON(tc.size)
			body := &upstreamBufferBody{reader: bytes.NewReader(data)}
			const contentType = "application/problem+json; charset=utf-8"
			s := &Server{client: &http.Client{Transport: niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Body: body, Header: http.Header{"Content-Type": {contentType}}}, nil
			})}}
			s.config.Admin.Enabled = true
			r, call := metered(httptest.NewRequest(http.MethodGet, "/", nil), "niutrans_document", 0)
			w := &niuTransJSONDownloadWriter{ResponseRecorder: httptest.NewRecorder(), limit: tc.limit, err: tc.cause}
			logs := captureLogs(t)
			var aborted any
			func() {
				defer func() { aborted = recover() }()
				s.niuTransDownload(w, r, "https://example.test/download", call)
			}()
			if (aborted != nil) != tc.wantAbort || (aborted != nil && aborted != http.ErrAbortHandler) {
				t.Fatalf("下载中断变化：panic=%v，预期中断=%v", aborted, tc.wantAbort)
			}
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != contentType || !bytes.Equal(w.Body.Bytes(), data[:tc.wantBytes]) {
				t.Fatalf("写出内容变化：状态=%d，类型=%q，长度=%d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
			}
			if body.read != len(data) || !body.closed {
				t.Fatalf("写出前未完整读取或未关闭上游：读取=%d，关闭=%v", body.read, body.closed)
			}
			wantCalls, wantErrors := int64(1), int64(0)
			if tc.wantAbort {
				wantErrors = 1
			}
			if errors.Is(tc.cause, context.Canceled) {
				wantCalls, wantErrors = 0, 0
			}
			if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != wantCalls || counts.errors != wantErrors {
				t.Fatalf("写出后的下载指标变化：%+v", counts)
			}
			if strings.Contains(w.Body.String()+logs.String(), privateFailure.Error()) {
				t.Fatal("写出错误泄露到响应或日志")
			}
		})
	}
}

func TestNiuTransDownloadJSONCopyAllocationBudget(t *testing.T) {
	result := testing.Benchmark(func(b *testing.B) { benchmarkNiuTransResponseBuffer(b, 32, true, true) })
	if got, budget := result.AllocedBytesPerOp(), int64(8<<10); got > budget {
		t.Fatalf("小 JSON 下载分配 %d B，预算 %d B", got, budget)
	}
}

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
