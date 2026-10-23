package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

type upstreamBufferBody struct {
	reader *bytes.Reader
	cause  error
	read   int
	closed bool
}

func (b *upstreamBufferBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	if b.reader.Len() == 0 && b.cause != nil {
		return n, b.cause
	}
	return n, err
}

func (b *upstreamBufferBody) Close() error { b.closed = true; return nil }

func TestUpstreamResponseBufferCompatibility(t *testing.T) {
	privateFailure := errors.New("synthetic-private-read-details")
	for _, tc := range []struct {
		name     string
		body     []byte
		length   int64
		status   int
		cause    error
		wantErr  string
		wantRead int
	}{
		{name: "准确长度", body: []byte(`{"data":"合成"}`), length: 17, wantRead: 17},
		{name: "未知长度", body: []byte(`{}`), length: -1, wantRead: 2},
		{name: "零长度", body: []byte(`{}`), wantRead: 2},
		{name: "少报长度", body: []byte(`{"data":true}`), length: 1, wantRead: 13},
		{name: "多报长度", body: []byte(`{}`), length: 1024, wantRead: 2},
		{name: "声明超限但正文合法", body: []byte(`{}`), length: contract.UpstreamResponseBytes + 1, wantRead: 2},
		{name: "空正文", length: 512, wantErr: "invalid upstream response"},
		{name: "无效JSON", body: []byte(`bad json`), length: 512, wantRead: 8, wantErr: "invalid upstream response"},
		{name: "尾部额外JSON", body: []byte(`{} {}`), length: 2, wantRead: 5, wantErr: "invalid upstream response"},
		{name: "少报长度实际超限", body: bytes.Repeat([]byte(" "), contract.UpstreamResponseBytes+10), length: 512, wantRead: contract.UpstreamResponseBytes + 1, wantErr: "invalid upstream response"},
		{name: "恰好上限", body: append([]byte(`{}`), bytes.Repeat([]byte(" "), contract.UpstreamResponseBytes-2)...), length: contract.UpstreamResponseBytes, wantRead: contract.UpstreamResponseBytes},
		{name: "拒绝状态", body: []byte(`synthetic-private-response`), length: 26, status: 500, wantErr: "upstream rejected request"},
		{name: "读取错误", body: []byte(`{}`), length: 512, cause: privateFailure, wantRead: 2, wantErr: "upstream transport failed"},
		{name: "截断", body: []byte(`{}`), length: 1024, cause: io.ErrUnexpectedEOF, wantRead: 2, wantErr: "upstream transport failed"},
		{name: "超时", body: []byte(`{}`), length: 512, cause: context.DeadlineExceeded, wantRead: 2, wantErr: "upstream transport failed"},
		{name: "取消", length: 512, cause: context.Canceled, wantErr: "upstream transport failed"},
		{name: "正文与EOF同时返回", body: append([]byte(`{}`), bytes.Repeat([]byte(" "), 510)...), length: 512, cause: io.EOF, wantRead: 512},
		{name: "尾部读取错误", body: []byte(`{} `), length: 512, cause: privateFailure, wantRead: 3, wantErr: "upstream transport failed"},
		{name: "未知长度读取错误", body: []byte(`{}`), length: -1, cause: privateFailure, wantRead: 2, wantErr: "upstream transport failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &upstreamBufferBody{reader: bytes.NewReader(tc.body), cause: tc.cause}
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			s := &Server{client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, ContentLength: tc.length, Body: body, Header: make(http.Header)}, nil
			})}}
			req, err := http.NewRequest(http.MethodGet, "https://example.test/data", nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.sendUpstream(req)
			if tc.wantErr == "" {
				if err != nil || !bytes.Equal(got, tc.body) {
					t.Fatalf("正文读取变化：长度=%d，错误=%v", len(got), err)
				}
			} else if err == nil || err.Error() != tc.wantErr || got != nil {
				t.Fatalf("错误分类或部分正文变化：长度=%d，错误=%v", len(got), err)
			}
			if tc.cause != nil && tc.cause != io.EOF && !errors.Is(err, tc.cause) {
				t.Fatal("读取错误链丢失")
			}
			if body.read != tc.wantRead || !body.closed {
				t.Fatalf("读取或关闭状态变化：读取=%d，预期=%d，已关闭=%v", body.read, tc.wantRead, body.closed)
			}
		})
	}
}

// previousSendUpstream 保留原读取方式，供相同 HTTP 调用与 JSON 校验的基准对照。
func previousSendUpstream(s *Server, req *http.Request) ([]byte, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, upstreamTransportError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("upstream rejected request")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, contract.UpstreamResponseBytes+1))
	if err != nil {
		return nil, upstreamTransportError{cause: err}
	}
	if len(body) > contract.UpstreamResponseBytes || !json.Valid(body) {
		return nil, errors.New("invalid upstream response")
	}
	return body, nil
}

func benchmarkUpstreamResponseBuffer(b *testing.B, size int, known, previous bool) {
	data := append([]byte(`{}`), bytes.Repeat([]byte(" "), size-2)...)
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	s := &Server{client: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})}}
	req, err := http.NewRequest(http.MethodGet, "https://example.test/data", nil)
	if err != nil {
		b.Fatal(err)
	}
	read := s.sendUpstream
	if previous {
		read = func(req *http.Request) ([]byte, error) { return previousSendUpstream(s, req) }
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		got, err := read(req)
		if err != nil || !bytes.Equal(got, data) {
			b.Fatalf("响应读取失败：长度=%d，错误=%v", len(got), err)
		}
	}
}

func TestUpstreamResponseBufferAllocationBudget(t *testing.T) {
	const size = 768 << 10
	result := testing.Benchmark(func(b *testing.B) { benchmarkUpstreamResponseBuffer(b, size, true, false) })
	if got, budget := result.AllocedBytesPerOp(), int64(size)*3/2+64<<10; got > budget {
		t.Fatalf("上游正文分配 %d B，预算为 %d B", got, budget)
	}
}

func TestUpstreamResponseBufferSmallAllocationBudget(t *testing.T) {
	const size = 32
	baseline := testing.Benchmark(func(b *testing.B) { benchmarkUpstreamResponseBuffer(b, size, true, true) })
	result := testing.Benchmark(func(b *testing.B) { benchmarkUpstreamResponseBuffer(b, size, true, false) })
	if got, budget := result.AllocedBytesPerOp(), baseline.AllocedBytesPerOp()+32; got > budget {
		t.Fatalf("小响应分配 %d B，原实现预算为 %d B", got, budget)
	}
}

func BenchmarkUpstreamResponseBuffer(b *testing.B) {
	for _, tc := range []struct {
		name            string
		size            int
		known, previous bool
	}{
		{"32B/原实现", 32, true, true}, {"32B/优化后", 32, true, false},
		{"511B/原实现", 511, true, true}, {"511B/优化后", 511, true, false},
		{"短响应/原实现", 512, true, true}, {"短响应/优化后", 512, true, false},
		{"768KiB/原实现", 768 << 10, true, true}, {"768KiB/优化后", 768 << 10, true, false},
		{"1MiB/原实现", 1 << 20, true, true}, {"1MiB/优化后", 1 << 20, true, false},
		{"未知长度/原实现", 768 << 10, false, true}, {"未知长度/优化后", 768 << 10, false, false},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkUpstreamResponseBuffer(b, tc.size, tc.known, tc.previous) })
	}
}
