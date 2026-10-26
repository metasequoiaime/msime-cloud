package server

import (
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestQQMarkdownLinesKeepNewlineSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, markdown string
		want           []qqSection
	}{
		{"混合换行和代码块", "# 文档\r\n导语\n## 正文\r\n第一行\r\n```go\r\n## 代码里的标题\r\n```\n最后一行\r", []qqSection{
			{Doc: "文档", Title: "概述", Text: "导语"},
			{Doc: "文档", Title: "正文", Text: "第一行\n```go\n## 代码里的标题\n```\n最后一行"},
		}},
		{"连续回车", "## 正文\r\n第一行\r\r\n第二行", []qqSection{{Doc: "synthetic", Title: "正文", Text: "第一行\r\n第二行"}}},
		{"行内回车", "## 正文\n第一行\r第二行", []qqSection{{Doc: "synthetic", Title: "正文", Text: "第一行\r第二行"}}},
		{"没有末尾换行", "## 正文\n内容", []qqSection{{Doc: "synthetic", Title: "正文", Text: "内容"}}},
		{"末尾空行", "## 正文\r\n内容\r\n\r\n", []qqSection{{Doc: "synthetic", Title: "正文", Text: "内容"}}},
		{"末尾标题", "## 正文\r\n内容\r\n## 空段落\r", []qqSection{{Doc: "synthetic", Title: "正文", Text: "内容"}}},
		{"孤立回车作为文档名", "## 正文\n内容\n# \r", []qqSection{{Doc: "\r", Title: "正文", Text: "内容"}}},
		{"孤立回车作为空段落标题", "## 正文\n内容\n## \r", []qqSection{{Doc: "synthetic", Title: "正文", Text: "内容"}}},
		{"CRLF 前只有空白的标题", "## 正文\n内容\n# \r\n", []qqSection{{Doc: "synthetic", Title: "正文", Text: "内容\n#"}}},
		{"CRLF 前保留额外回车的标题", "## 正文\n内容\n# \r\r\n", []qqSection{{Doc: "\r", Title: "正文", Text: "内容"}}},
		{"空文档", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqSplitMarkdown("synthetic", tc.markdown); !slices.Equal(got, tc.want) {
				t.Fatalf("段落不兼容：结果=%+v，期望=%+v", got, tc.want)
			}
		})
	}
}

func TestQQMarkdownLinesMatchNormalizedInput(t *testing.T) {
	random := rand.New(rand.NewSource(4))
	lines := []string{"# 文档", "## 第一节", "### 小节", "普通正文", "```", "```go", "", " \t", "\xff\xfe", "内容\r中间"}
	endings := []string{"\n", "\r\n", "\n\n", ""}
	for range 300 {
		var markdown strings.Builder
		for range 30 {
			markdown.WriteString(lines[random.Intn(len(lines))])
			markdown.WriteString(endings[random.Intn(len(endings))])
		}
		if random.Intn(2) == 0 {
			markdown.WriteByte('\r')
		}
		text := markdown.String()
		want := qqSplitMarkdown("synthetic", strings.ReplaceAll(text, "\r\n", "\n"))
		if got := qqSplitMarkdown("synthetic", text); !slices.Equal(got, want) {
			t.Fatalf("换行归一化不兼容：text=%q，结果=%+v，期望=%+v", text, got, want)
		}
	}
}

func TestQQMarkdownLinesAvoidsFullLineSlice(t *testing.T) {
	for _, tc := range []struct{ name, ending string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			markdown := "## 正文" + tc.ending + strings.Repeat(tc.ending, 65536)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			sections := qqSplitMarkdown("synthetic", markdown)
			runtime.ReadMemStats(&after)
			if len(sections) != 0 {
				t.Fatalf("空白正文不应形成段落：%d", len(sections))
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("65,536 行空白正文分配 %d 字节", allocated)
			// 预算容许正文 builder 正常扩容，但不能再为全部行分配字符串切片。
			if allocated > 768<<10 {
				t.Fatalf("逐行解析分配 %d 字节，超过 768 KiB 的预算", allocated)
			}
		})
	}
}

func BenchmarkQQMarkdownLines(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"many_blank_lf", strings.Repeat("\n", 65536)},
		{"many_blank_crlf", strings.Repeat("\r\n", 65536)},
		{"many_text_crlf", strings.Repeat("普通正文\r\n", 16384)},
		{"long_line_crlf", strings.Repeat("水", 65536) + "\r\n"},
	} {
		markdown := "## 正文\r\n" + tc.body
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSplitMarkdown("synthetic", markdown)
			}
		})
	}
}
