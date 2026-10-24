package boundedio_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
)

func TestReadAllSizedBoundsAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hint, limit int64
		size        int
	}{
		{"空正文", 0, 8, 0},
		{"零上限", 4, 0, 8},
		{"小正文精确容量", 8, 16, 8},
		{"大正文精确容量", 4096, 8192, 4096},
		{"恰好上限", 2048, 2048, 2048},
		{"少报长度", 8, 2048, 1024},
		{"多报长度", 2048, 4096, 1024},
		{"非零声明空正文", 8, 16, 0},
		{"未知长度", -1, 2048, 1024},
		{"零声明长度", 0, 2048, 1024},
		{"超限声明短正文", 4096, 2048, 8},
		{"少报长度超限", 8, 2048, 4096},
		{"准确声明超限", 4096, 2048, 4096},
		{"未知长度超限", -1, 2048, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &boundedReader{data: strings.Repeat("x", tc.size), end: io.EOF}
			got, err := boundedio.ReadAllSized(reader, tc.hint, tc.limit)
			want := reader.data[:min(tc.size, int(tc.limit)+1)]
			if err != nil || got == nil || string(got) != want || reader.read != len(want) || reader.closed {
				t.Fatalf("正文、读取上限或生命周期变化：长度=%d，已读=%d，关闭=%v，错误=%v", len(got), reader.read, reader.closed, err)
			}
			if tc.hint > 0 && tc.hint <= tc.limit && int64(tc.size) == tc.hint && cap(got) != tc.size {
				t.Fatalf("准确声明长度的正文不应保留额外缓存容量：长度=%d，容量=%d", len(got), cap(got))
			}
		})
	}
}

func TestReadAllSizedPreservesReadErrors(t *testing.T) {
	failure := errors.New("synthetic read failure")
	for _, hint := range []int64{-1, 0, 8, 512, 1024, 4096} {
		for _, size := range []int{0, 4, 8, 512, 1024, 2049} {
			reader := &boundedReader{data: strings.Repeat("x", size), end: failure}
			got, err := boundedio.ReadAllSized(reader, hint, 2048)
			if !errors.Is(err, failure) || string(got) != reader.data || reader.closed {
				t.Fatalf("丢失同时返回的正文或错误：声明=%d，实际=%d，返回=%d，错误=%v", hint, size, len(got), err)
			}
		}
	}
}
