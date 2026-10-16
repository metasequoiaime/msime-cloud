package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unsafe"
)

func TestQQSnippetStructuredScanCandidate(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"普通正文，没有 Markdown 标记", false},
		{"**加粗**", true},
		{"- 标签：说明", true},
		{"[链接](https://example.test)", true},
	} {
		if got := qqSnippetNeedsStructuredScan(tc.input); got != tc.want {
			t.Errorf("qqSnippetNeedsStructuredScan(%q) = %v，预期 %v", tc.input, got, tc.want)
		}
	}
}

func TestQQSnippetStructuredScanMatchesContainsAny(t *testing.T) {
	random := rand.New(rand.NewSource(19))
	const markers = "*-_:：[]`>#|"
	for size := 0; size <= 256; size++ {
		data := make([]byte, size)
		if _, err := random.Read(data); err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if got, want := qqSnippetNeedsStructuredScan(text), strings.ContainsAny(text, markers); got != want {
			t.Fatalf("结构化摘要扫描不兼容：长度=%d，结果=%v，期望=%v", size, got, want)
		}
	}
}

func TestQQSentencePrefixKeepsPreviousSemantics(t *testing.T) {
	previous := func(text string, limit int) string {
		runes := []rune(text)
		if len(runes) <= limit {
			return text
		}
		kept := runes[:max(limit-1, 0)]
		for i := len(kept) - 1; i >= limit/2; i-- {
			if strings.ContainsRune(qqSentenceBreaks, kept[i]) {
				kept = kept[:i+1]
				break
			}
		}
		return strings.TrimRightFunc(string(kept), unicode.IsSpace) + "…"
	}
	run := func(fn func(string, int) string, text string, limit int) (result string, panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		return fn(text, limit), false
	}
	texts := []string{"", "水杉。第一句！第二句；\n后面还有文字", "a😀b\u0301 c", "水\xff\xfe杉", "\xed\xa0\x80x", "\xe4\xb8", "a\x00b", "水杉\u3000 \t\n"}
	random := rand.New(rand.NewSource(2))
	for size := 1; size <= 64; size++ {
		data := make([]byte, size)
		if _, err := random.Read(data); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(data))
	}
	for _, text := range texts {
		for limit := -4; limit <= len([]rune(text))+1; limit++ {
			want, wantPanic := run(previous, text, limit)
			got, gotPanic := run(qqTruncateSentence, text, limit)
			if got != want || gotPanic != wantPanic {
				t.Fatalf("按句截断不兼容：text=%q，limit=%d，结果=(%q,%v)，期望=(%q,%v)", text, limit, got, gotPanic, want, wantPanic)
			}
		}
	}
}

func TestQQSentencePrefixAvoidsAllocationWithoutTruncation(t *testing.T) {
	text := strings.Repeat("水", 300)
	var got string
	if allocations := testing.AllocsPerRun(20, func() { got = qqTruncateSentence(text, 300) }); allocations != 0 {
		t.Fatalf("未截断正文仍分配 %.0f 次，期望直接返回原文", allocations)
	}
	if got != text {
		t.Fatal("未截断正文发生变化")
	}
}

func TestQQMarkdownPrefixKeepsEncodingAndLength(t *testing.T) {
	for _, body := range []string{
		strings.Repeat("水", qqSectionRunes-1),
		strings.Repeat("水", qqSectionRunes),
		strings.Repeat("水", qqSectionRunes+1),
		strings.Repeat("😀", 65536),
		strings.Repeat("水", qqSectionRunes-1) + "\xff",
		strings.Repeat("水", qqSectionRunes-1) + "\xff\xfe尾",
	} {
		want := body
		if runes := []rune(body); len(runes) > qqSectionRunes {
			want = string(runes[:qqSectionRunes])
		}
		sections := qqSplitMarkdown("synthetic", "# 合成文档\n## 正文\n"+body+"\n")
		if len(sections) != 1 || sections[0].Text != want || sections[0].Title != "正文" || sections[0].Doc != "合成文档" {
			t.Fatalf("段落截断不兼容：正文长度=%d，段落数=%d", len(body), len(sections))
		}
	}
}

func TestQQSnippetPrefixKeepsSummarySelection(t *testing.T) {
	long := strings.Repeat("水", 65536)
	want := strings.Repeat("水", qqSnippetRunes) + "…"
	for _, text := range []string{long, "**" + long + "**", "前面的说明 **" + long + "** 后面的说明"} {
		if got := qqSnippet(text); got != want {
			t.Fatalf("长摘要不兼容：长度=%d，结果=%q", len(text), got)
		}
	}
}

func TestQQMarkdownHeadingsReleaseSourceDocument(t *testing.T) {
	markdown := "# 合成文档\n## 正文\n" + strings.Repeat("水", 65536)
	sections := qqSplitMarkdown("synthetic", markdown)
	if len(sections) != 1 {
		t.Fatalf("合成文档应只有一个段落：%d", len(sections))
	}
	for _, field := range []struct {
		name, got, want string
	}{
		{"文档名", sections[0].Doc, "合成文档"},
		{"段落标题", sections[0].Title, "正文"},
	} {
		if field.got != field.want {
			t.Fatalf("%s=%q，期望 %q", field.name, field.got, field.want)
		}
		// 比较真实字段的底层地址，验证短标题不会使缓存持有整份 Markdown。
		source := markdown[strings.Index(markdown, field.want):]
		if unsafe.StringData(field.got) == unsafe.StringData(source) {
			t.Errorf("%s仍引用原始 Markdown，导致整份长正文无法释放", field.name)
		}
	}
}

func BenchmarkQQKnowledgePrefix(b *testing.B) {
	long := strings.Repeat("水", 65536)
	exact := strings.Repeat("水", 300)
	markdown := "# 合成文档\n## 正文\n" + long + "\n"
	for _, tc := range []struct {
		name string
		fn   func() string
	}{
		{"sentence", func() string { return qqTruncateSentence(long, 300) }},
		{"sentence_exact", func() string { return qqTruncateSentence(exact, 300) }},
		{"snippet", func() string { return qqSnippet(long) }},
		{"markdown", func() string { return qqSplitMarkdown("synthetic", markdown)[0].Text }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var got string
			for b.Loop() {
				got = tc.fn()
			}
			if got == "" {
				b.Fatal("知识库前缀为空")
			}
		})
	}
}
