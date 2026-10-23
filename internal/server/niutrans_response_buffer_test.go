package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func niuTransBufferJSON(size int) []byte {
	return append([]byte(`{"code":200}`), bytes.Repeat([]byte(" "), size-len(`{"code":200}`))...)
}

func TestNiuTransResponseBufferCompatibility(t *testing.T) {
	privateFailure := errors.New("synthetic-private-read-details")
	for _, download := range []bool{false, true} {
		path := "请求"
		if download {
			path = "下载"
		}
		for _, tc := range []struct {
			name   string
			body   []byte
			length int64
			status int
			cause  error
		}{
			{name: "小响应", body: niuTransBufferJSON(32), length: 32},
			{name: "准确长度", body: niuTransBufferJSON(768 << 10), length: 768 << 10},
			{name: "未知长度", body: niuTransBufferJSON(512), length: -1},
			{name: "零长度", body: niuTransBufferJSON(512)},
			{name: "少报长度", body: niuTransBufferJSON(4096), length: 512},
			{name: "多报长度", body: niuTransBufferJSON(32), length: 4096},
			{name: "声明超过JSON上限", body: niuTransBufferJSON(32), length: niuTransResponseBytes + 1},
			{name: "极大声明", body: niuTransBufferJSON(32), length: 1<<63 - 1},
			{name: "恰好上限", body: niuTransBufferJSON(niuTransResponseBytes), length: niuTransResponseBytes},
			{name: "实际超限", body: niuTransBufferJSON(niuTransResponseBytes + 32), length: 512},
			{name: "空正文", length: 512},
			{name: "无效JSON", body: []byte("synthetic-private-invalid-json"), length: 512},
			{name: "拒绝状态", body: niuTransBufferJSON(512), length: 512, status: 500},
			{name: "数据与EOF", body: niuTransBufferJSON(512), length: 512, cause: io.EOF},
			{name: "数据与错误", body: niuTransBufferJSON(512), length: 512, cause: privateFailure},
			{name: "截断", body: niuTransBufferJSON(512), length: 1024, cause: io.ErrUnexpectedEOF},
			{name: "超时", body: niuTransBufferJSON(512), length: 512, cause: context.DeadlineExceeded},
			{name: "取消", body: niuTransBufferJSON(512), length: 512, cause: context.Canceled},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				body := &upstreamBufferBody{reader: bytes.NewReader(tc.body), cause: tc.cause}
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				const contentType = "Application/JSON; charset=utf-8"
				s := &Server{client: &http.Client{Transport: niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, ContentLength: tc.length, Body: body, Header: http.Header{"Content-Type": {contentType}}}, nil
				})}}
				s.config.Admin.Enabled = true
				r, call := metered(httptest.NewRequest(http.MethodGet, "/", nil), "niutrans_document", 0)
				readFailure := tc.cause != nil && tc.cause != io.EOF
				accepted := status == 200 && !readFailure && len(tc.body) <= niuTransResponseBytes
				wantRead := min(len(tc.body), niuTransResponseBytes+1)
				w := httptest.NewRecorder()
				logs := captureLogs(t)
				if download {
					accepted = accepted && niuTransMediaAccepted(tc.body) && tc.length <= niuTransDownloadBytes
					if status != 200 || tc.length > niuTransDownloadBytes {
						wantRead = 0
					}
					s.niuTransDownload(w, r, "https://example.test/download", call)
					if accepted {
						if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tc.body) || w.Header().Get("Content-Type") != contentType {
							t.Fatalf("成功下载变化：状态=%d，长度=%d", w.Code, w.Body.Len())
						}
					} else {
						wantStatus := http.StatusBadGateway
						if errors.Is(tc.cause, context.DeadlineExceeded) {
							wantStatus = http.StatusGatewayTimeout
						}
						if w.Code != wantStatus || bytes.Equal(w.Body.Bytes(), tc.body) {
							t.Fatalf("下载失败响应变化：状态=%d", w.Code)
						}
					}
				} else {
					got, kind, err := s.niuTransRequestWithType(r, "https://example.test/status", http.MethodGet, "", nil)
					s.settleMeter(call, err == nil)
					if accepted {
						if err != nil || !bytes.Equal(got, tc.body) || kind != contentType {
							t.Fatalf("成功交换变化：长度=%d，类型=%q，错误=%v", len(got), kind, err)
						}
					} else {
						if err == nil || got != nil || kind != "" {
							t.Fatalf("失败时保留部分响应：长度=%d，错误=%v", len(got), err)
						}
						if readFailure && !errors.Is(err, tc.cause) {
							t.Fatal("读取错误链丢失")
						}
						if !readFailure && len(tc.body) > niuTransResponseBytes && err.Error() != "upstream response too large" {
							t.Fatalf("超限分类变化：%v", err)
						}
						if !readFailure && status != 200 && !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatalf("状态错误分类变化：%v", err)
						}
						upstreamError(w, r, err)
					}
				}
				if body.read != wantRead || !body.closed {
					t.Fatalf("读取或关闭变化：读取=%d，预期=%d，关闭=%v", body.read, wantRead, body.closed)
				}
				wantCalls, wantErrors := int64(1), int64(0)
				if !accepted {
					wantErrors = 1
				}
				if errors.Is(tc.cause, context.Canceled) {
					wantCalls, wantErrors = 0, 0
				}
				if counts := pendingCalls(&s.metrics, "niutrans_document"); counts.calls != wantCalls || counts.errors != wantErrors {
					t.Fatalf("指标变化：%+v", counts)
				}
				if strings.Contains(w.Body.String()+logs.String(), "synthetic-private-") {
					t.Fatal("响应或日志泄露上游详情")
				}
			})
		}
	}
}

func benchmarkNiuTransResponseBuffer(b *testing.B, size int, known, download bool) {
	data := niuTransBufferJSON(size)
	length := int64(-1)
	if known {
		length = int64(size)
	}
	s := &Server{client: &http.Client{Transport: niuTransDownloadTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: length, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		if download {
			req, call := metered(r, "niutrans_document", 0)
			w := &niuTransDownloadWriter{header: make(http.Header)}
			s.niuTransDownload(w, req, "https://example.test/download", call)
			if w.status != 200 || w.bytes != int64(size) {
				b.Fatalf("下载失败：状态=%d，字节=%d", w.status, w.bytes)
			}
		} else {
			got, _, err := s.sendNiuTrans(r, "https://example.test/status", http.MethodGet, "", nil)
			if err != nil || !bytes.Equal(got, data) {
				b.Fatalf("请求失败：长度=%d，错误=%v", len(got), err)
			}
		}
	}
}

func TestNiuTransResponseBufferAllocationBudget(t *testing.T) {
	for _, download := range []bool{false, true} {
		name := "请求"
		if download {
			name = "下载"
		}
		t.Run(name, func(t *testing.T) {
			const size = 768 << 10
			result := testing.Benchmark(func(b *testing.B) { benchmarkNiuTransResponseBuffer(b, size, true, download) })
			if got, budget := result.AllocedBytesPerOp(), int64(size)*3/2+64<<10; got > budget {
				t.Fatalf("响应读取分配 %d B，预算 %d B", got, budget)
			}
		})
	}
}

func BenchmarkNiuTransResponseBuffer(b *testing.B) {
	for _, download := range []bool{false, true} {
		name := "请求"
		if download {
			name = "下载"
		}
		b.Run(name, func(b *testing.B) {
			for _, tc := range []struct {
				name  string
				size  int
				known bool
			}{
				{"32B", 32, true}, {"511B", 511, true}, {"512B", 512, true},
				{"768KiB", 768 << 10, true}, {"1MiB", 1 << 20, true}, {"未知长度", 768 << 10, false},
			} {
				b.Run(tc.name, func(b *testing.B) { benchmarkNiuTransResponseBuffer(b, tc.size, tc.known, download) })
			}
		})
	}
}
