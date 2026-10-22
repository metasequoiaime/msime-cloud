package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

type qqMediaBufferBody struct {
	reader  *bytes.Reader
	chunk   int
	readErr error
	read    int
	closed  bool
}

func (r *qqMediaBufferBody) Read(p []byte) (int, error) {
	if r.chunk > 0 && len(p) > r.chunk {
		p = p[:r.chunk]
	}
	n, err := r.reader.Read(p)
	r.read += n
	if err == io.EOF && r.readErr != nil {
		return n, r.readErr
	}
	return n, err
}

func (r *qqMediaBufferBody) Close() error {
	r.closed = true
	return nil
}

func qqMediaBufferBot(body io.ReadCloser, length int64, status int) *qqBot {
	return &qqBot{
		mediaHosts: []string{"example.test"},
		mediaClient: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, ContentLength: length, Header: make(http.Header), Body: body}, nil
		})},
	}
}

func TestQQMediaDownloadBufferBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		length   int64
		limit    int64
		status   int
		readErr  error
		wantErr  string
		wantRead int
	}{
		{"空附件", "", 0, 8, http.StatusOK, nil, "", 0},
		{"已知长度", "合成附件", 12, 12, http.StatusOK, nil, "", 12},
		{"未知长度", "合成附件", -1, 12, http.StatusOK, nil, "", 12},
		{"恰好上限", "12345678", -1, 8, http.StatusOK, nil, "", 8},
		{"少报长度", "12345678", 1, 8, http.StatusOK, nil, "", 8},
		{"多报长度", "12", 8, 8, http.StatusOK, nil, "", 2},
		{"少报长度后扩容", string(bytes.Repeat([]byte("x"), 8192)), 1, 8192, http.StatusOK, nil, "", 8192},
		{"少报长度后扩容超限", string(bytes.Repeat([]byte("x"), 8200)), 1, 8192, http.StatusOK, nil, "attachment too large", 8193},
		{"未知长度超限", "123456789tail", -1, 8, http.StatusOK, nil, "attachment too large", 9},
		{"少报长度超限", "123456789tail", 1, 8, http.StatusOK, nil, "attachment too large", 9},
		{"声明长度超限", "123456789", 9, 8, http.StatusOK, nil, "attachment too large", 0},
		{"状态失败", "synthetic-private-body", -1, 8, http.StatusForbidden, nil, "attachment download answered 403", 0},
		{"读取失败", "12", -1, 8, http.StatusOK, errors.New("synthetic-private-error"), "attachment download failed", 2},
		{"已知长度读取失败", "12", 8, 8, http.StatusOK, errors.New("synthetic-private-error"), "attachment download failed", 2},
		{"零上限空附件", "", 0, 0, http.StatusOK, nil, "", 0},
		{"零上限非空附件", "1tail", -1, 0, http.StatusOK, nil, "attachment too large", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &qqMediaBufferBody{reader: bytes.NewReader([]byte(tc.body)), chunk: 3, readErr: tc.readErr}
			bot := qqMediaBufferBot(body, tc.length, tc.status)
			got, err := bot.downloadMedia(context.Background(), "https://example.test/synthetic", tc.limit)
			if tc.wantErr == "" {
				if err != nil || got == nil || !bytes.Equal(got, []byte(tc.body)) {
					t.Fatalf("附件内容变化：长度=%d，错误=%v", len(got), err)
				}
			} else if err == nil || err.Error() != tc.wantErr || got != nil {
				t.Fatalf("错误分类变化：长度=%d，错误=%v，期望=%s", len(got), err, tc.wantErr)
			}
			if !body.closed || body.read != tc.wantRead {
				t.Fatalf("读取范围或关闭状态变化：已读=%d，期望=%d，关闭=%v", body.read, tc.wantRead, body.closed)
			}
		})
	}
}

func benchmarkQQMediaDownloadBuffer(b *testing.B, data []byte, length int64, limit int64) {
	bot := qqMediaBufferBot(nil, length, http.StatusOK)
	bot.mediaClient.Transport = qqTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Header: make(http.Header),
			Body: &qqMediaBufferBody{reader: bytes.NewReader(data), chunk: 4096}}, nil
	})
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		got, err := bot.downloadMedia(context.Background(), "https://example.test/synthetic", limit)
		if err != nil || !bytes.Equal(got, data) {
			b.Fatalf("附件读取不正确：长度=%d，错误=%v", len(got), err)
		}
	}
}

func TestQQMediaDownloadBufferAllocationBudget(t *testing.T) {
	data := bytes.Repeat([]byte("合成附件"), 128<<10)
	result := testing.Benchmark(func(b *testing.B) { benchmarkQQMediaDownloadBuffer(b, data, int64(len(data)), qqImageBytes) })
	// 已知长度只需附件本身和 HTTP 开销，不应再分配中间块及完整拼接副本。
	if got, budget := result.AllocedBytesPerOp(), int64(len(data))*3/2+64<<10; got > budget {
		t.Fatalf("已知长度的附件读取分配 %d B，预算为 %d B", got, budget)
	}
}

func BenchmarkQQMediaDownloadBuffer(b *testing.B) {
	for _, tc := range []struct {
		name   string
		size   int
		length bool
	}{
		{"短附件", 512, true},
		{"1536KiB已知长度", 1536 << 10, true},
		{"1536KiB未知长度", 1536 << 10, false},
		{"20MiB已知长度", qqImageBytes, true},
		{"20MiB未知长度", qqImageBytes, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			data := bytes.Repeat([]byte("x"), tc.size)
			length := int64(-1)
			if tc.length {
				length = int64(len(data))
			}
			benchmarkQQMediaDownloadBuffer(b, data, length, qqVideoBytes)
		})
	}
}
