package publictools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

type publicBufferTransport func(*http.Request) (*http.Response, error)

func (f publicBufferTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type publicBufferBody struct {
	reader *bytes.Reader
	cause  error
	read   int
	closed bool
}

func (b *publicBufferBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	if b.reader.Len() == 0 && b.cause != nil {
		return n, b.cause
	}
	return n, err
}

func (b *publicBufferBody) Close() error { b.closed = true; return nil }

func TestHTTPResponseBufferCompatibility(t *testing.T) {
	failure := errors.New("synthetic-private-read-error")
	for _, tc := range []struct {
		name     string
		data     []byte
		length   int64
		status   int
		cause    error
		wantErr  string
		wantRead int
	}{
		{name: "小响应", data: []byte(`{}`), length: 2, wantRead: 2},
		{name: "预分配响应", data: bytes.Repeat([]byte("x"), 512), length: 512, wantRead: 512},
		{name: "未知长度", data: []byte(`{}`), length: -1, wantRead: 2},
		{name: "零长度", data: []byte(`{}`), wantRead: 2},
		{name: "少报长度", data: bytes.Repeat([]byte("x"), 600), length: 512, wantRead: 600},
		{name: "多报长度", data: []byte(`{}`), length: 1024, wantRead: 2},
		{name: "声明超限但正文合法", data: []byte(`{}`), length: MaxResponse + 1, wantRead: 2},
		{name: "零长度空响应"},
		{name: "声明非零的空响应", length: 512},
		{name: "原始正文保留", data: []byte("not json"), length: 512, wantRead: 8},
		{name: "恰好上限", data: bytes.Repeat([]byte("x"), MaxResponse), length: MaxResponse, wantRead: MaxResponse},
		{name: "实际超限", data: bytes.Repeat([]byte("x"), MaxResponse+10), length: 512, wantRead: MaxResponse + 1, wantErr: "public_api_response_too_large_or_unreadable"},
		{name: "状态失败", data: []byte("synthetic-private-body"), length: 512, status: 429, wantErr: "public_api_status_429"},
		{name: "正文伴随错误", data: []byte(`{}`), length: 512, cause: failure, wantRead: 2, wantErr: "public_api_response_too_large_or_unreadable"},
		{name: "未知长度读取错误", data: []byte(`{}`), length: -1, cause: failure, wantRead: 2, wantErr: "public_api_response_too_large_or_unreadable"},
		{name: "读取超时", data: []byte(`{}`), length: 512, cause: context.DeadlineExceeded, wantRead: 2, wantErr: "public_api_response_too_large_or_unreadable"},
		{name: "截断", data: []byte(`{}`), length: 512, cause: io.ErrUnexpectedEOF, wantRead: 2, wantErr: "public_api_response_too_large_or_unreadable"},
		{name: "正文伴随EOF", data: bytes.Repeat([]byte("x"), 512), length: 512, cause: io.EOF, wantRead: 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &publicBufferBody{reader: bytes.NewReader(tc.data), cause: tc.cause}
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			client := &http.Client{Transport: publicBufferTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, ContentLength: tc.length, Header: make(http.Header), Body: body}, nil
			})}
			execute, err := HTTP(client, "https://example.test", "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := execute(t.Context(), Request{Method: http.MethodGet, Path: "/data"})
			if tc.wantErr == "" {
				if err != nil || got == nil || !bytes.Equal(got, tc.data) {
					t.Fatalf("正文变化：长度=%d，错误=%v", len(got), err)
				}
			} else if err == nil || err.Error() != tc.wantErr || got != nil {
				t.Fatalf("错误分类或部分正文变化：长度=%d，错误=%v", len(got), err)
			}
			if body.read != tc.wantRead || !body.closed {
				t.Fatalf("读取范围或关闭状态变化：读取=%d，预期=%d，关闭=%v", body.read, tc.wantRead, body.closed)
			}
		})
	}
}

func benchmarkHTTPResponseBuffer(b *testing.B, size int, known bool) {
	data := bytes.Repeat([]byte("x"), size)
	length := int64(-1)
	if known {
		length = int64(size)
	}
	client := &http.Client{Transport: publicBufferTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	execute, err := HTTP(client, "https://example.test", "")
	if err != nil {
		b.Fatal(err)
	}
	request := Request{Method: http.MethodGet, Path: "/data"}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		got, err := execute(context.Background(), request)
		if err != nil || !bytes.Equal(got, data) {
			b.Fatalf("响应读取失败：长度=%d，错误=%v", len(got), err)
		}
	}
}

func TestHTTPResponseBufferAllocationBudget(t *testing.T) {
	const size = 768 << 10
	result := testing.Benchmark(func(b *testing.B) { benchmarkHTTPResponseBuffer(b, size, true) })
	if got, budget := result.AllocedBytesPerOp(), int64(size)*3/2+64<<10; got > budget {
		t.Fatalf("公共响应分配 %d B，预算为 %d B", got, budget)
	}
}

func TestHTTPResponseBufferSmallAllocationBudget(t *testing.T) {
	baseline := testing.Benchmark(func(b *testing.B) { benchmarkHTTPResponseBuffer(b, 32, false) })
	result := testing.Benchmark(func(b *testing.B) { benchmarkHTTPResponseBuffer(b, 32, true) })
	if got, budget := result.AllocedBytesPerOp(), baseline.AllocedBytesPerOp()+32; got > budget {
		t.Fatalf("小响应分配 %d B，原读取预算为 %d B", got, budget)
	}
}

func BenchmarkHTTPResponseBuffer(b *testing.B) {
	for _, tc := range []struct {
		name  string
		size  int
		known bool
	}{
		{"32B已知长度", 32, true}, {"32B未知长度", 32, false},
		{"512B已知长度", 512, true}, {"768KiB已知长度", 768 << 10, true},
		{"768KiB未知长度", 768 << 10, false}, {"1MiB已知长度", MaxResponse, true},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkHTTPResponseBuffer(b, tc.size, tc.known) })
	}
}
