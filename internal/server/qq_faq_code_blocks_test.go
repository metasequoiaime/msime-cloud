package server

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// 固定原有匹配规则，供代码块边界、比较语义和开销对照使用。
var qqCodeBlockPattern = regexp.MustCompile("```[^\n]*\n[\\s\\S]*?```")

func previousQQSameCodeBlocks(a, b string) bool {
	return strings.Join(qqCodeBlockPattern.FindAllString(a, -1), "\n") == strings.Join(qqCodeBlockPattern.FindAllString(b, -1), "\n")
}

func faqCodeBlockSamples() []string {
	texts := []string{
		"", "普通正文", "```", "```plain", "```\n", "```\n```", "````\n````", "``````\n```", "```a```b\nx```",
		"前缀```go\nx\n```后缀", "```go\r\nx\r\n```", "```go\rx```", "```\u2028```", "```\n```\n```\n```",
		"```\n未闭合", "```\n```\n```\n尾部", "```\xff\n\xfe\x00```", "```\n\ufffd```", "```\n\xe4\xb8```",
		strings.Repeat("```x\ny```译文", 200), "```text\n" + strings.Repeat("x", 2000) + "```",
	}
	rng := rand.New(rand.NewSource(684))
	atoms := []string{"合成正文", "譯文", "`", "``", "```", "````", "```go", "```\n", "\n```", "\n", "\r", " ", "\t", "\u2028", "\xff", "\xe4\xb8", "\ufffd", "𐐀"}
	for range 4000 {
		var text strings.Builder
		for range rng.Intn(80) {
			text.WriteString(atoms[rng.Intn(len(atoms))])
		}
		texts = append(texts, text.String())
	}
	for range 1000 {
		raw := make([]byte, rng.Intn(200))
		_, _ = rng.Read(raw)
		texts = append(texts, "```\n"+string(raw)+"```\n```\n尾部```")
	}
	return texts
}

func TestQQNextCodeBlockMatchesRegexp(t *testing.T) {
	for _, text := range faqCodeBlockSamples() {
		var got []string
		for rest := text; rest != ""; {
			var block string
			block, rest = qqNextCodeBlock(rest)
			if block == "" {
				break
			}
			got = append(got, block)
		}
		if want := qqCodeBlockPattern.FindAllString(text, -1); !reflect.DeepEqual(got, want) {
			t.Fatalf("输入=%q，代码块=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQSameCodeBlocksMatchesPrevious(t *testing.T) {
	texts := faqCodeBlockSamples()
	for i, text := range texts {
		for _, other := range []string{
			text, texts[(i+1)%len(texts)], strings.ReplaceAll(text, "\r\n", "\n"),
			"譯文" + strings.Join(qqCodeBlockPattern.FindAllString(text, -1), "\n譯文\n") + "結尾",
			text + "```新增\nx```",
		} {
			if got, want := qqSameCodeBlocks(text, other), previousQQSameCodeBlocks(text, other); got != want {
				t.Fatalf("原文=%q，译文=%q，相同=%t，期望=%t", text, other, got, want)
			}
		}
	}
}

func TestQQSameCodeBlocksAllocations(t *testing.T) {
	for _, pair := range [][2]string{
		{"普通正文", "普通譯文"},
		{strings.Repeat("```x\ny```原文", 200), strings.Repeat("```x\ny```譯文", 200)},
		{"```x\n" + strings.Repeat("正文", 2000) + "```", "```x\n" + strings.Repeat("正文", 2000) + "```"},
		{"```x\n不同```", "```x\n變化```"},
	} {
		var got bool
		if allocated := testing.AllocsPerRun(20, func() { got = qqSameCodeBlocks(pair[0], pair[1]) }); allocated != 0 {
			t.Fatalf("代码块比较分配 %g 次，期望零分配", allocated)
		}
		if want := previousQQSameCodeBlocks(pair[0], pair[1]); got != want {
			t.Fatal("代码块比较语义变化")
		}
	}
}

func TestQQFAQCodeBlockValidationMatchesPrevious(t *testing.T) {
	prose := strings.Repeat("合成答案。", 20)
	for _, pair := range [][2]string{
		{"```x\na```", "```x\na```"}, {"```x\na```", "```x\nb```"},
		{"```x\r\na```", "```x\na```"}, {"```x\na```", ""},
		{"```x\n未闭合", "普通译文"}, {"```a```b\nx```", "```a```b\nx```"},
		{strings.Repeat("```x\ny```\n", 200), strings.Repeat("```x\ny```譯文", 200)},
	} {
		entry := faqValidationEntry(prose + pair[0])
		entry.AnswerTW = prose + pair[1]
		s := faqValidationServer(t, entry)
		got, gotErr := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		want, wantErr := s.previousQQSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		if got != want || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Fatalf("校验结果变化：%v，期望 %v", gotErr, wantErr)
		}
	}
}

func TestQQFAQCodeBlockValidationAllocationBudget(t *testing.T) {
	answer := strings.Repeat("合成答案。", 20) + strings.Repeat("```\nx\n```\n", 200)
	s := faqValidationServer(t, faqValidationEntry(answer))
	var got qqFAQEntry
	var err error
	allocated := testing.AllocsPerRun(5, func() {
		got, err = s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
	})
	if err != nil || got.Answer != strings.TrimSpace(answer) {
		t.Fatalf("FAQ 校验结果变化：%v", err)
	}
	// 保留模拟模型请求、JSON 编解码和链接清理的预算，禁止逐代码块建立匹配切片。
	if allocated > 128 {
		t.Fatalf("200 个代码块的 FAQ 校验分配 %g 次，超过 128 次预算", allocated)
	}
}

func BenchmarkQQFAQCodeBlockValidation(b *testing.B) {
	for _, tc := range []struct{ name, answer string }{
		{"ManyBlocks", strings.Repeat("合成答案。", 20) + strings.Repeat("```\nx\n```\n", 200)},
		{"LongBlock", strings.Repeat("合成答案。", 20) + "```text\n" + strings.Repeat("x", 2000) + "\n```"},
	} {
		s := faqValidationServer(b, faqValidationEntry(tc.answer))
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				entry, err := s.qqSummariseFAQ(b.Context(), "合成问答", []string{"合成分类"})
				if err != nil || entry.Answer == "" {
					b.Fatalf("校验失败：%v", err)
				}
			}
		})
	}
}

var faqCodeBlockComparisonResult bool

func BenchmarkQQSameCodeBlocks(b *testing.B) {
	for _, tc := range []struct{ name, a, c string }{
		{"NoBlocks", strings.Repeat("普通正文", 500), strings.Repeat("普通譯文", 500)},
		{"ManyBlocks", strings.Repeat("```x\ny```原文", 200), strings.Repeat("```x\ny```譯文", 200)},
		{"LongBlock", "```text\n" + strings.Repeat("x", 2000) + "```", "```text\n" + strings.Repeat("x", 2000) + "```"},
		{"FirstMismatch", "```text\na```" + strings.Repeat("```x\ny```", 200), "```text\nb```" + strings.Repeat("```x\ny```", 200)},
	} {
		for _, impl := range []struct {
			name  string
			equal func(string, string) bool
		}{
			{"Previous", previousQQSameCodeBlocks}, {"Current", qqSameCodeBlocks},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					faqCodeBlockComparisonResult = impl.equal(tc.a, tc.c)
				}
			})
		}
	}
}
