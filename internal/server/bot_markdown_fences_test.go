package server

import (
	"slices"
	"strings"
	"testing"
)

func qqMarkdownFenceCases() []struct{ name, block string } {
	return []struct{ name, block string }{
		{"长反引号围栏中的短围栏", "````markdown\n```\n## 代码标题\n代码正文\n```\n````"},
		{"波浪号围栏", "~~~markdown\n## 代码标题\n代码正文\n~~~"},
		{"不同字符不能关闭围栏", "~~~markdown\n```\n## 代码标题\n代码正文\n~~~"},
		{"反引号围栏中的波浪号", "```markdown\n~~~\n## 代码标题\n代码正文\n```"},
		{"关闭行不能带说明文字", "```markdown\n```text\n## 代码标题\n代码正文\n```"},
		{"更长的关闭围栏", "```markdown\n## 代码标题\n代码正文\n`````"},
		{"波浪号围栏中的短围栏", "~~~~markdown\n~~~\n## 代码标题\n代码正文\n~~~~"},
	}
}

func TestQQMarkdownFenceKeepsInlineCode(t *testing.T) {
	for _, inline := range []string{"``", "```行内代码```", "````行内代码````"} {
		t.Run(inline, func(t *testing.T) {
			markdown := "## 示例\n" + inline + "\n## 后续\n正文"
			want := []qqSection{
				{Doc: "synthetic", Title: "示例", Text: inline},
				{Doc: "synthetic", Title: "后续", Text: "正文"},
			}
			if got := qqSplitMarkdown("synthetic", markdown); !slices.Equal(got, want) {
				t.Errorf("行内反引号遮住了后续段落：%+v", got)
			}
			wantPlain := strings.ReplaceAll(inline, "`", "")
			if got := qqPlainText(inline); got != wantPlain {
				t.Errorf("行内反引号被当作代码块：%q，期望=%q", got, wantPlain)
			}
			if got := qqReleaseNotes(inline); got != wantPlain {
				t.Errorf("发布说明中的行内反引号被当作代码块：%q，期望=%q", got, wantPlain)
			}
		})
	}
}

func TestQQMarkdownFenceBoundaries(t *testing.T) {
	for _, tc := range qqMarkdownFenceCases() {
		t.Run(tc.name, func(t *testing.T) {
			markdown := "# 合成文档\n## 示例\n开头\n" + tc.block + "\n尾部\n## 后续\n下一节"
			want := []qqSection{
				{Doc: "合成文档", Title: "示例", Text: "开头\n" + tc.block + "\n尾部"},
				{Doc: "合成文档", Title: "后续", Text: "下一节"},
			}
			if got := qqSplitMarkdown("synthetic", markdown); !slices.Equal(got, want) {
				t.Fatalf("代码块改变了知识库段落：结果=%+v，期望=%+v", got, want)
			}
		})
	}
}

func TestQQMarkdownFenceSummariesExcludeCode(t *testing.T) {
	for _, tc := range qqMarkdownFenceCases() {
		t.Run(tc.name, func(t *testing.T) {
			markdown := "开头\n" + tc.block + "\n尾部"
			if got := qqPlainText(markdown); got != "开头 尾部" {
				t.Errorf("纯文本摘要包含代码或遗漏正文：%q", got)
			}
			if got := qqReleaseNotes(markdown); got != "开头；尾部" {
				t.Errorf("发布说明包含代码或遗漏正文：%q", got)
			}
		})
	}
}

func TestQQMarkdownFenceUnclosedSummaries(t *testing.T) {
	for _, fence := range []string{"```", "````", "~~~", "~~~~"} {
		t.Run(fence, func(t *testing.T) {
			if got := qqPlainText(fence); got != "" {
				t.Errorf("单行未关闭围栏应被过滤：%q", got)
			}
			markdown := "开头\n" + fence + "text\n## 代码标题\n代码正文"
			if got := qqPlainText(markdown); got != "开头" {
				t.Errorf("未关闭代码块进入纯文本摘要：%q", got)
			}
			if got := qqReleaseNotes(markdown); got != "开头" {
				t.Errorf("未关闭代码块进入发布说明：%q", got)
			}
			if got := qqSplitMarkdown("synthetic", "## 示例\n"+markdown); len(got) != 1 || got[0].Title != "示例" || !strings.Contains(got[0].Text, "## 代码标题") {
				t.Errorf("未关闭代码块中的标题被当作段落：%+v", got)
			}
		})
	}
}
