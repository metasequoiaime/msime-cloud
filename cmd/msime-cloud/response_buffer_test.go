package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
)

type cliResponseBufferBody struct {
	reader *bytes.Reader
	cause  error
	read   int
	closed bool
}

func (b *cliResponseBufferBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	if b.reader.Len() == 0 && b.cause != nil {
		return n, b.cause
	}
	return n, err
}

func (b *cliResponseBufferBody) Close() error { b.closed = true; return nil }

func TestCLIResponseBufferCompatibility(t *testing.T) {
	privateFailure := errors.New("synthetic-private-read-details")
	for _, tc := range []struct {
		name   string
		data   []byte
		length int64
		status int
		kind   string
		cause  error
	}{
		{name: "小响应", data: bytes.Repeat([]byte("x"), 32), length: 32},
		{name: "准确长度", data: bytes.Repeat([]byte("x"), 2<<20), length: 2 << 20},
		{name: "未知长度", data: []byte("synthetic"), length: -1},
		{name: "零长度", data: []byte("synthetic")},
		{name: "少报长度", data: bytes.Repeat([]byte("x"), 4096), length: 512},
		{name: "多报长度", data: []byte("synthetic"), length: 4096},
		{name: "声明超限", data: []byte("synthetic"), length: maxCLIResponseBytes + 1},
		{name: "极大声明", data: []byte("synthetic"), length: 1<<63 - 1},
		{name: "恰好上限", data: bytes.Repeat([]byte("x"), maxCLIResponseBytes), length: maxCLIResponseBytes},
		{name: "实际超限", data: bytes.Repeat([]byte("x"), maxCLIResponseBytes+32), length: 512},
		{name: "空正文", length: 512},
		{name: "无效JSON原样输出", data: []byte("invalid json"), length: 512, kind: "application/json"},
		{name: "失败状态仍输出正文", data: []byte("synthetic failure"), length: 512, status: 500},
		{name: "失败状态读取错误优先", data: []byte("synthetic"), length: 512, status: 500, cause: privateFailure},
		{name: "数据与EOF", data: bytes.Repeat([]byte("x"), 512), length: 512, cause: io.EOF},
		{name: "数据与读取错误", data: bytes.Repeat([]byte("x"), 512), length: 512, cause: privateFailure},
		{name: "截断", data: []byte("synthetic"), length: 1024, cause: io.ErrUnexpectedEOF},
		{name: "超时", data: []byte("synthetic"), length: 512, cause: context.DeadlineExceeded},
		{name: "取消", length: 512, cause: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &cliResponseBufferBody{reader: bytes.NewReader(tc.data), cause: tc.cause}
			status, kind := tc.status, tc.kind
			if status == 0 {
				status = http.StatusOK
			}
			if kind == "" {
				kind = "text/plain; charset=utf-8"
			}
			var stdout bytes.Buffer
			c := cli{stdout: &stdout}
			err := c.print(&http.Response{StatusCode: status, Status: fmt.Sprint(status), ContentLength: tc.length, Body: body, Header: http.Header{"Content-Type": {kind}}}, "")
			readFailure := tc.cause != nil && tc.cause != io.EOF
			oversize := len(tc.data) > maxCLIResponseBytes
			if readFailure {
				if !errors.Is(err, tc.cause) || stdout.Len() != 0 {
					t.Fatalf("读取失败处理变化：输出=%d，错误=%v", stdout.Len(), err)
				}
			} else if oversize {
				if err == nil || err.Error() != fmt.Sprintf("response exceeds %d bytes", maxCLIResponseBytes) || stdout.Len() != 0 {
					t.Fatalf("超限处理变化：输出=%d，错误=%v", stdout.Len(), err)
				}
			} else {
				if status == http.StatusOK {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var statusErr statusError
					if !errors.As(err, &statusErr) || statusErr.status != fmt.Sprint(status) {
						t.Fatalf("状态错误变化：%v", err)
					}
				}
				if len(tc.data) == 0 {
					if stdout.Len() != 0 {
						t.Fatal("空正文产生额外输出")
					}
				} else if stdout.Len() != len(tc.data)+1 || !bytes.Equal(stdout.Bytes()[:len(tc.data)], tc.data) || stdout.Bytes()[len(tc.data)] != '\n' {
					t.Fatalf("正文输出变化：长度=%d", stdout.Len())
				}
			}
			if body.read != min(len(tc.data), maxCLIResponseBytes+1) || !body.closed {
				t.Fatalf("读取或关闭变化：读取=%d，关闭=%v", body.read, body.closed)
			}
		})
	}
}

type cliResponseBufferTransport func(*http.Request) (*http.Response, error)

func (f cliResponseBufferTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func benchmarkCLIResponseBuffer(b *testing.B, size int, known, postJSON bool) {
	data := bytes.Repeat([]byte("x"), size)
	kind := "text/plain"
	if postJSON {
		data = append([]byte(`{"ok":true}`), bytes.Repeat([]byte(" "), size-len(`{"ok":true}`))...)
		kind = "application/json"
	}
	length := int64(-1)
	if known {
		length = int64(size)
	}
	response := func() *http.Response {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Type": {kind}}}
	}
	c := cli{stdout: io.Discard, client: &http.Client{Transport: cliResponseBufferTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			r.Body.Close()
		}
		return response(), nil
	})}}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		if postJSON {
			var result struct {
				OK bool `json:"ok"`
			}
			if err := c.postJSON("https://example.test", "/v1/auth/challenges", nil, &result); err != nil || !result.OK {
				b.Fatalf("JSON 控制响应失败：%v", err)
			}
		} else if err := c.print(response(), ""); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCLIResponseBufferAllocationBudget(t *testing.T) {
	for _, postJSON := range []bool{false, true} {
		name := "文本输出"
		if postJSON {
			name = "JSON请求"
		}
		t.Run(name, func(t *testing.T) {
			const size = 8 << 20
			result := testing.Benchmark(func(b *testing.B) { benchmarkCLIResponseBuffer(b, size, true, postJSON) })
			budget := int64(size)*9/2 + 128<<10
			if postJSON {
				budget = int64(size)*3/2 + 128<<10
			}
			if got := result.AllocedBytesPerOp(); got > budget {
				t.Fatalf("控制响应分配 %d B，预算 %d B", got, budget)
			}
		})
	}
}

func BenchmarkCLIResponseBuffer(b *testing.B) {
	for _, postJSON := range []bool{false, true} {
		name := "文本输出"
		if postJSON {
			name = "JSON请求"
		}
		b.Run(name, func(b *testing.B) {
			for _, tc := range []struct {
				name  string
				size  int
				known bool
			}{
				{"32B", 32, true}, {"512B", 512, true}, {"8MiB", 8 << 20, true}, {"16MiB", 16 << 20, true}, {"未知长度", 8 << 20, false},
			} {
				b.Run(tc.name, func(b *testing.B) { benchmarkCLIResponseBuffer(b, tc.size, tc.known, postJSON) })
			}
		})
	}
}
