package server

import (
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestQQMarkdownBodyKeepsTrimAndTruncationOrder(t *testing.T) {
	padding := strings.Repeat(" \t\u3000\n", 8192)
	texts := []string{
		padding + "短正文" + padding,
		strings.Repeat("水", qqSectionRunes-1) + "\n" + padding,
		strings.Repeat("水", qqSectionRunes-1) + "\n" + padding + "尾部",
		"正文" + strings.Repeat(" ", qqSectionRunes) + "尾部",
		strings.Repeat("水", qqSectionRunes-1) + "\xff" + padding,
		strings.Repeat("水", qqSectionRunes-1) + "\xff" + padding + "尾部",
		strings.Repeat("😀", qqSectionRunes-1) + "\xe4\xb8\n尾部",
		padding,
	}
	random := rand.New(rand.NewSource(5))
	units := []string{"水", "😀", "a", " ", "\t", "\r", "\n", "\u3000", "\xff", "\xe4\xb8"}
	for range 80 {
		var body strings.Builder
		for range qqSectionRunes + random.Intn(100) {
			body.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, body.String())
	}
	for _, body := range texts {
		want := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
		if runes := []rune(want); len(runes) > qqSectionRunes {
			want = string(runes[:qqSectionRunes])
		}
		sections := qqSplitMarkdown("synthetic", "## 正文\n"+body)
		if want == "" {
			if len(sections) != 0 {
				t.Fatalf("空白正文不应形成段落：%d", len(sections))
			}
			continue
		}
		if len(sections) != 1 || sections[0].Text != want {
			t.Fatalf("正文裁剪不兼容：输入字节数=%d，段落数=%d，期望正文=%q", len(body), len(sections), want)
		}
	}
}

func TestQQMarkdownBodyContinuesParsingAfterLimit(t *testing.T) {
	long := strings.Repeat("水", qqSectionRunes+100)
	markdown := "被忽略的前言\n# 文档\n## 正文\n" + long + "\n```go\n## 代码中的标题\n```\n## 后续\n短正文\n"
	want := []qqSection{
		{Doc: "文档", Title: "正文", Text: strings.Repeat("水", qqSectionRunes)},
		{Doc: "文档", Title: "后续", Text: "短正文"},
	}
	if got := qqSplitMarkdown("synthetic", markdown); !slices.Equal(got, want) {
		t.Fatalf("超长段落后的标题或代码块解析发生变化：%+v", got)
	}
}

func TestQQMarkdownBodyUsesBoundedAllocation(t *testing.T) {
	long := strings.Repeat("水", 1<<20)
	padding := strings.Repeat(" \t\u3000", 1<<18)
	for _, tc := range []struct {
		name, markdown, want string
	}{
		{"超长正文", "## 正文\n" + long, strings.Repeat("水", qqSectionRunes)},
		{"未归入标题的前言", long + "\n## 正文\n短正文", "短正文"},
		{"巨大首尾空白", "## 正文\n" + padding + "短正文" + padding, "短正文"},
		{"多行超长正文", "## 正文\n" + strings.Repeat("普通正文\n", 65536), strings.Repeat("普通正文\n", qqSectionRunes/5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			sections := qqSplitMarkdown("synthetic", tc.markdown)
			runtime.ReadMemStats(&after)
			if len(sections) != 1 || sections[0].Text != tc.want {
				t.Fatalf("合成正文解析结果不正确：段落数=%d", len(sections))
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("解析分配 %d 字节", allocated)
			// 输入在测量前构造；预算允许保留正文及运行时开销，但不能累积整段长正文。
			if allocated > 256<<10 {
				t.Fatalf("解析分配 %d 字节，超过 256 KiB 的预算", allocated)
			}
		})
	}
}

func BenchmarkQQMarkdownBody(b *testing.B) {
	for _, tc := range []struct{ name, markdown string }{
		{"long_body", "## 正文\n" + strings.Repeat("水", 1<<20)},
		{"long_preface", strings.Repeat("水", 1<<20) + "\n## 正文\n短正文"},
		{"long_padding", "## 正文\n" + strings.Repeat(" \t\u3000", 1<<18) + "短正文"},
		{"many_lines", "## 正文\n" + strings.Repeat("普通正文\n", 65536)},
		{"short", "## 正文\n短正文"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSplitMarkdown("synthetic", tc.markdown)
			}
		})
	}
}
