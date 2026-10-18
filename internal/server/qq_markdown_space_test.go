package server

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

func TestQQMarkdownLeadingSpaceFastPathMatchesUnicodeTrim(t *testing.T) {
	for _, tc := range []struct {
		name, input string
	}{
		{"plain", "普通正文"},
		{"ascii", " \t\r\n\f\v普通正文"},
		{"unicode", "\u3000\u0085\u00a0普通正文"},
		{"invalid_utf8", " \xff正文"},
		{"all_space", " \t\u3000"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.TrimLeftFunc(tc.input, unicode.IsSpace)
			if got := qqMarkdownLeadingSpace(tc.input); got != want {
				t.Fatalf("行首空白裁剪不兼容：结果=%q，预期=%q", got, want)
			}
		})
	}
}

func TestQQMarkdownLeadingSpaceFastPathCoversUnicodeSpaces(t *testing.T) {
	for _, r := range []rune{'\u0085', '\u00a0', '\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000'} {
		input := string(r) + "正文"
		if got, want := qqMarkdownLeadingSpace(input), strings.TrimLeftFunc(input, unicode.IsSpace); got != want {
			t.Fatalf("Unicode 空白裁剪不兼容：r=%U，结果=%q，预期=%q", r, got, want)
		}
	}
}

func TestQQMarkdownSpaceClassifierMatchesUnicode(t *testing.T) {
	for _, r := range []rune{'\t', '\n', '\v', '\f', '\r', ' ', '\u0085', '\u00a0', '\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '水', '\u200b', '\ufeff', '\ufffd'} {
		if got, want := qqMarkdownSpaceIsSpace(r), unicode.IsSpace(r); got != want {
			t.Fatalf("空白分类不兼容：r=%U，结果=%v，预期=%v", r, got, want)
		}
	}
}

func TestQQTrimSpaceMatchesUnicodeTrim(t *testing.T) {
	for _, input := range []string{
		"",
		" \t\u3000正文\u00a0\n",
		"\u0085\u2000正文\u2029",
		" \xff正文\u3000",
		" \t\u3000",
	} {
		if got, want := qqTrimSpace(input), strings.TrimSpace(input); got != want {
			t.Fatalf("空白裁剪不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQMarkdownLeadingSpaceKeepsIndentation(t *testing.T) {
	for _, prefix := range []string{"", " ", "\t", "\r", "\u0085", "\u00a0", "\u1680", "\u2000", "\u200a", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", "\u200b", "\ufeff", "\xff"} {
		body := prefix + "首行\n" + prefix + "第二行"
		sections := qqSplitMarkdown("synthetic", "## 正文\n"+body)
		if len(sections) != 1 || sections[0].Text != strings.TrimSpace(body) {
			t.Fatalf("正文缩进发生变化：prefix=%q，结果=%+v", prefix, sections)
		}
	}
}

func TestQQMarkdownLeadingSpaceKeepsFenceAndHeadingRules(t *testing.T) {
	for _, prefix := range []string{" ", "\t", "\u3000\u0085\t"} {
		body := "开头\n" + prefix + "```go \u3000\n## 代码中的标题\n" + prefix + "``` \u00a0"
		markdown := "## 正文\n" + body + "\n## 后续\n" + prefix + "短正文\n"
		want := []qqSection{
			{Doc: "synthetic", Title: "正文", Text: body},
			{Doc: "synthetic", Title: "后续", Text: "短正文"},
		}
		want[0].Text = strings.TrimSpace(want[0].Text)
		if got := qqSplitMarkdown("synthetic", markdown); !slices.Equal(got, want) {
			t.Fatalf("缩进代码块解析发生变化：prefix=%q，结果=%+v", prefix, got)
		}
		markdown = "## 正文\n开头\n" + prefix + "## 普通正文\n"
		want = []qqSection{{Doc: "synthetic", Title: "正文", Text: "开头\n" + prefix + "## 普通正文"}}
		if got := qqSplitMarkdown("synthetic", markdown); !slices.Equal(got, want) {
			t.Fatalf("带缩进的正文被当成标题：prefix=%q，结果=%+v", prefix, got)
		}
	}
}

func BenchmarkQQMarkdownSpace(b *testing.B) {
	unicodePadding := strings.Repeat(" \t\u3000", 1<<18)
	for _, tc := range []struct{ name, body string }{
		{"unicode_leading", unicodePadding + "短正文"},
		{"unicode_trailing", "短正文" + unicodePadding},
		{"ascii_leading", strings.Repeat(" \t", 1<<20) + "短正文"},
		{"ordinary_lines", strings.Repeat("普通正文\n", 16384)},
		{"short", "短正文"},
	} {
		markdown := "## 正文\n" + tc.body
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSplitMarkdown("synthetic", markdown)
			}
		})
	}
}
