package boundedio_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
)

func TestReadBufferBoundsAndHints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hint  int64
		limit int64
		size  int
	}{
		{"空正文", -1, 2048, 0},
		{"零上限", -1, 0, 100},
		{"小提示", 511, 2048, 100},
		{"最小提示", 512, 2048, 512},
		{"准确提示", 1024, 2048, 1024},
		{"多报长度", 1536, 2048, 100},
		{"少报长度", 512, 2048, 1536},
		{"正好上限", 2048, 2048, 2048},
		{"实际超限", 512, 2048, 4096},
		{"未知长度", -1, 2048, 4096},
		{"提示超限", 4096, 2048, 100},
		{"极大提示", 1<<63 - 1, 2048, 100},
		{"极小提示", -1 << 63, 2048, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &boundedReader{data: strings.Repeat("x", tc.size), end: io.EOF}
			var buffer bytes.Buffer
			err := boundedio.ReadBuffer(&buffer, body, tc.hint, tc.limit)
			want := body.data[:min(tc.size, int(tc.limit)+1)]
			if err != nil || buffer.String() != want || body.read != len(want) || body.closed {
				t.Fatalf("读取或正文所有权变化：长度=%d，读取=%d，关闭=%t，错误=%v", buffer.Len(), body.read, body.closed, err)
			}
			if tc.hint >= bytes.MinRead && tc.hint <= tc.limit && int64(tc.size) <= tc.hint && buffer.Cap() != int(tc.hint)+bytes.MinRead {
				t.Fatalf("容量提示未保留 EOF 探测空间：容量=%d，提示=%d", buffer.Cap(), tc.hint)
			}
		})
	}
}

func TestReadBufferReusesAndResetsStorage(t *testing.T) {
	storage := make([]byte, 8192)
	buffer := bytes.NewBuffer(storage)
	buffer.Next(100)
	for _, hint := range []int64{4096, -1, 1<<63 - 1} {
		if err := boundedio.ReadBuffer(buffer, strings.NewReader("new"), hint, 4096); err != nil {
			t.Fatal(err)
		}
		if buffer.String() != "new" || buffer.Cap() != len(storage) || &buffer.Bytes()[0] != &storage[0] {
			t.Fatal("已有缓冲没有清空并复用原存储")
		}
	}
}

func TestReadBufferReplacesSmallStorage(t *testing.T) {
	buffer := bytes.NewBuffer(make([]byte, 32, 512))
	if err := boundedio.ReadBuffer(buffer, strings.NewReader("new"), 4096, 8192); err != nil {
		t.Fatal(err)
	}
	if buffer.String() != "new" || buffer.Cap() != 4096+bytes.MinRead {
		t.Fatalf("小缓冲替换后容量或内容变化：容量=%d，内容=%q", buffer.Cap(), buffer.String())
	}
}

func TestReadBufferPreservesPartialDataAndErrors(t *testing.T) {
	cause := errors.New("synthetic read failure")
	for _, hint := range []int64{-1, 512, 2048, 4096} {
		for _, size := range []int{0, 100, 2049} {
			body := &boundedReader{data: strings.Repeat("x", size), end: cause}
			buffer := bytes.NewBufferString("old")
			err := boundedio.ReadBuffer(buffer, body, hint, 2048)
			if !errors.Is(err, cause) || buffer.String() != body.data || body.closed {
				t.Fatalf("部分正文或错误丢失：提示=%d，长度=%d，错误=%v", hint, buffer.Len(), err)
			}
		}
	}
}
