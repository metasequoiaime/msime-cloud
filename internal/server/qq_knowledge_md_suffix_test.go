package server

import (
	"strings"
	"testing"
)

func previousQQMarkdownFileName(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".md")
}

func TestQQMarkdownFileNameCompatibility(t *testing.T) {
	for _, name := range []string{"faq.md", "FAQ.MD", "guide.Md", "guide.txt", "md", ".md", "说明.MＤ", "\xff.MD"} {
		if got, want := qqMarkdownFileName(name), previousQQMarkdownFileName(name); got != want {
			t.Errorf("文件名=%q：结果 %t，原结果 %t", name, got, want)
		}
	}
}

var qqMarkdownFileNameBenchmarkResult bool

func BenchmarkQQMarkdownFileName(b *testing.B) {
	for _, name := range []string{"guide.md", "GUIDE.MD", "说明文档.md", "guide.txt"} {
		b.Run(name+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqMarkdownFileNameBenchmarkResult = previousQQMarkdownFileName(name)
			}
		})
		b.Run(name+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqMarkdownFileNameBenchmarkResult = qqMarkdownFileName(name)
			}
		})
	}
}
