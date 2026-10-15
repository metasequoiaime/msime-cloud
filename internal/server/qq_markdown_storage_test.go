package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQMarkdownTrimmedBodiesKeepContent(t *testing.T) {
	padding := strings.Repeat(" \t\u3000\n", 8192)
	for _, tc := range []struct{ name, body, want string }{
		{"前置空白", padding + "正文", "正文"},
		{"后置空白", "正文" + padding, "正文"},
		{"两端空白", padding + "正文" + padding, "正文"},
		{"保留内部空白", padding + "正文  \n\n尾部" + padding, "正文  \n\n尾部"},
		{"未截断的无效编码", padding + "水\xff杉" + padding, "水\xff杉"},
		{"恰好达到上限", padding + strings.Repeat("水", qqSectionRunes) + padding, strings.Repeat("水", qqSectionRunes)},
		{"截断后保留前缀空白", "正文" + strings.Repeat(" ", qqSectionRunes) + "尾部", "正文" + strings.Repeat(" ", qqSectionRunes-2)},
		{"全部为空白", padding, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sections := qqSplitMarkdown("synthetic", "# 合成文档\n## 正文\n"+tc.body)
			if tc.want == "" {
				if len(sections) != 0 {
					t.Fatalf("空白正文不应形成段落：%d", len(sections))
				}
				return
			}
			if len(sections) != 1 || sections[0].Text != tc.want || sections[0].Doc != "合成文档" || sections[0].Title != "正文" {
				t.Fatalf("段落正文不兼容：段落数=%d，期望正文长度=%d", len(sections), len(tc.want))
			}
		})
	}
}

func TestQQMarkdownTrimmedBodiesReleasePadding(t *testing.T) {
	padding := strings.Repeat(" ", 1<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var cached []qqSection
	for i := range 16 {
		body := padding + "正文"
		if i%2 == 0 {
			body = "正文" + padding
		}
		cached = append(cached, qqSplitMarkdown("synthetic", "# 合成文档\n## 正文\n"+body)...)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// 保持测试输入和缓存存活，只让解析过程中的临时字符串被 GC 回收。
	runtime.KeepAlive(padding)
	if len(cached) != 16 {
		t.Fatalf("缓存段落数=%d，期望 16", len(cached))
	}
	for _, section := range cached {
		if section.Text != "正文" {
			t.Fatalf("缓存正文发生变化：%q", section.Text)
		}
	}
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("16 个短段落在 GC 后仍占用 %d 字节", retained)
	// 给运行时和平台差异留出余量，但不能继续保留每段 1 MiB 的首尾空白。
	if retained > 8<<20 {
		t.Fatalf("短正文仍保留大段空白的底层缓冲：%d 字节，期望小于 8 MiB", retained)
	}
	runtime.KeepAlive(cached)
}

func BenchmarkQQMarkdownStorage(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"short", "正文"},
		{"short_ascii", "a"},
		{"padded_short", strings.Repeat(" ", 1<<20) + "正文"},
	} {
		markdown := "# 合成文档\n## 正文\n" + tc.body
		want := strings.TrimSpace(tc.body)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var sections []qqSection
			for b.Loop() {
				sections = qqSplitMarkdown("synthetic", markdown)
			}
			if len(sections) != 1 || sections[0].Text != want {
				b.Fatal("段落正文不正确")
			}
		})
	}
}
