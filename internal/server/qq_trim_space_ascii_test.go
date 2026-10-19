package server

import (
	"strings"
	"testing"
)

func TestQQTrimSpaceASCIIBlockBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, 2, 31, 32, 33, 63, 64, 65, 127, 128, 129, 1024} {
		for _, padding := range []string{
			strings.Repeat(" ", size),
			strings.Repeat(" \t", size),
			strings.Repeat(" \t", size) + strings.Repeat(" ", size),
			strings.Repeat(" ", size) + "\t\r\n\f\v\u3000\u0085",
		} {
			for _, body := range []string{"", "正文", "a b", "\u200b", "\xff", "\xe4\xb8", "\x00尾部"} {
				for _, text := range []string{padding + body, body + padding, padding + body + padding} {
					if got, want := qqTrimSpace(text), strings.TrimSpace(text); got != want {
						t.Fatalf("块边界=%d，输入=%q，裁剪=%q，期望=%q", size, text, got, want)
					}
				}
			}
		}
	}
}

func BenchmarkQQTrimSpaceASCII(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"普通短句", "普通正文"},
		{"短空白", " \t正文\n"},
		{"纯空格前缀", strings.Repeat(" ", 1<<20) + "正文"},
		{"交替空白前缀", strings.Repeat(" \t", 1<<19) + "正文"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := strings.TrimSpace(tc.text)
			b.ReportAllocs()
			for b.Loop() {
				if qqTrimSpace(tc.text) != want {
					b.Fatal("裁剪结果错误")
				}
			}
		})
	}
}
