package httpclient_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

type responseBodyReader struct {
	io.Reader
	closed bool
}

func (r *responseBodyReader) Close() error {
	r.closed = true
	return nil
}

func TestReadResponseBodyDelegatesWithoutChangingResponse(t *testing.T) {
	failure := errors.New("合成正文读取错误")
	data := strings.Repeat("x", 1024)
	for _, limit := range []int64{0, 512, 2048} {
		body := &responseBodyReader{Reader: io.MultiReader(strings.NewReader(data), iotest.ErrReader(failure))}
		response := &http.Response{StatusCode: http.StatusBadGateway, ContentLength: 512, Body: body}
		got, err := httpclient.ReadResponseBody(response, limit)
		want := data[:min(len(data), int(limit)+1)]
		if string(got) != want || (limit < int64(len(data)) && err != nil) || (limit >= int64(len(data)) && !errors.Is(err, failure)) {
			t.Fatalf("上限=%d，读取结果长度=%d，错误=%v", limit, len(got), err)
		}
		if body.closed || response.Body != body || response.StatusCode != http.StatusBadGateway || response.ContentLength != 512 {
			t.Fatal("读取器改变了调用方负责的响应状态或生命周期")
		}
	}
}
