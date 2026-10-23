package httpclient_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

type responseBodyReader struct {
	data   string
	read   int
	closed bool
	end    error
}

func (r *responseBodyReader) Read(p []byte) (int, error) {
	if r.read == len(r.data) {
		return 0, r.end
	}
	n := copy(p[:min(len(p), 17)], r.data[r.read:])
	r.read += n
	if r.read == len(r.data) {
		return n, r.end
	}
	return n, nil
}

func (r *responseBodyReader) Close() error {
	r.closed = true
	return nil
}

func TestReadResponseBodyBoundsAndLengthHints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length int64
		limit  int64
		size   int
	}{
		{"空正文", -1, 2048, 0},
		{"零上限", -1, 0, 100},
		{"小正文", 100, 2048, 100},
		{"分配阈值之前", 511, 2048, 511},
		{"分配阈值", 512, 2048, 512},
		{"准确长度", 1024, 2048, 1024},
		{"少报长度", 512, 2048, 1024},
		{"多报长度", 1536, 2048, 1024},
		{"声明长度超过上限", 4096, 2048, 100},
		{"未知长度", -1, 2048, 1024},
		{"未声明长度", 0, 2048, 1024},
		{"正好上限", 2048, 2048, 2048},
		{"超过上限", 2048, 2048, 4096},
		{"少报长度仍受上限限制", 512, 2048, 4096},
		{"未知长度仍受上限限制", -1, 2048, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &responseBodyReader{data: strings.Repeat("x", tc.size), end: io.EOF}
			response := &http.Response{StatusCode: http.StatusBadGateway, ContentLength: tc.length, Body: body}
			got, err := httpclient.ReadResponseBody(response, tc.limit)
			want := body.data[:min(tc.size, int(tc.limit)+1)]
			if err != nil || string(got) != want || body.read != len(want) {
				t.Fatalf("读取结果变化：长度=%d，已读取=%d，期望长度=%d，错误=%v", len(got), body.read, len(want), err)
			}
			if body.closed || response.StatusCode != http.StatusBadGateway || response.ContentLength != tc.length {
				t.Fatal("读取器改变了调用方负责的响应状态或生命周期")
			}
		})
	}
}

func TestReadResponseBodyPreservesDataWithReadError(t *testing.T) {
	readError := errors.New("synthetic body read failure")
	for _, length := range []int64{-1, 0, 512, 1024, 4096} {
		for _, size := range []int{0, 100, 1024} {
			body := &responseBodyReader{data: strings.Repeat("x", size), end: readError}
			response := &http.Response{ContentLength: length, Body: body}
			got, err := httpclient.ReadResponseBody(response, 2048)
			if !errors.Is(err, readError) || string(got) != body.data || body.closed {
				t.Errorf("丢失读取错误或部分正文：声明长度=%d，实际长度=%d，返回长度=%d，错误=%v", length, size, len(got), err)
			}
		}
	}
}
