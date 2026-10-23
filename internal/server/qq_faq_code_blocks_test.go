package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 固定原有匹配规则，供代码块边界、比较语义和开销对照使用。
var qqCodeBlockPattern = regexp.MustCompile("```[^\n]*\n[\\s\\S]*?```")

func previousQQSameCodeBlocks(a, b string) bool {
	return strings.Join(qqCodeBlockPattern.FindAllString(a, -1), "\n") == strings.Join(qqCodeBlockPattern.FindAllString(b, -1), "\n")
}

func TestQQNextCodeBlockFences(t *testing.T) {
	for _, tc := range qqMarkdownFenceCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, tail := range []string{"", "\n尾部正文", "\n~~~text\n后续代码\n~~~"} {
				block, rest, valid := qqNextCodeBlock("前导正文\n" + tc.block + tail)
				if !valid || block != tc.block || rest != tail {
					t.Fatalf("围栏提取错误：代码=%q，剩余=%q，有效=%t", block, rest, valid)
				}
			}
		})
	}
	for _, text := range []string{"", "普通正文", "``", "```行内代码```", "前缀```text\n普通正文", "````行内代码````"} {
		if block, rest, valid := qqNextCodeBlock(text); !valid || block != "" || rest != "" {
			t.Fatalf("普通正文被当作代码围栏：%q -> %q/%q/%t", text, block, rest, valid)
		}
	}
	for _, text := range []string{"```", "~~~text", "````\n```", "~~~\n```\n未关闭"} {
		if _, _, valid := qqNextCodeBlock(text); valid {
			t.Fatalf("未关闭围栏被当作有效输入：%q", text)
		}
	}
}

func TestQQNextCodeBlockPreservesRawBody(t *testing.T) {
	random := rand.New(rand.NewSource(688))
	for range 1000 {
		raw := make([]byte, random.Intn(200))
		_, _ = random.Read(raw)
		// 围栏比整个正文更长，任意字节或换行都不能成为关闭行。
		marker := strings.Repeat("`", len(raw)+3)
		want := marker + "text\n" + string(raw) + "\n" + marker
		block, rest, valid := qqNextCodeBlock("原文\n" + want + "\n尾部")
		if !valid || block != want || rest != "\n尾部" || !qqSameCodeBlocks(want, "譯文\n"+want) {
			t.Fatalf("代码原文发生变化：输入=%q，代码=%q，剩余=%q，有效=%t", want, block, rest, valid)
		}
	}
}

func TestQQSameCodeBlocksMatchesPreviousForClosedBackticks(t *testing.T) {
	random := rand.New(rand.NewSource(684))
	for range 1000 {
		var a, b strings.Builder
		for range random.Intn(10) {
			block := "```text\n合成命令 " + strings.Repeat("x", random.Intn(50)) + "\n```"
			a.WriteString("原文\n" + block + "\n")
			b.WriteString("譯文\n" + block + "\n")
		}
		first, second := a.String(), b.String()
		for _, other := range []string{second, strings.ReplaceAll(second, "x", "y"), second + "```text\n新增命令\n```"} {
			if got, want := qqSameCodeBlocks(first, other), previousQQSameCodeBlocks(first, other); got != want {
				t.Fatalf("普通闭合围栏比较变化：原文=%q，译文=%q，结果=%t，期望=%t", first, other, got, want)
			}
		}
	}
}

func TestQQSameCodeBlocksAllocations(t *testing.T) {
	for _, pair := range [][2]string{
		{"普通正文", "普通譯文"},
		{strings.Repeat("```x\ny\n```\n原文\n", 200), strings.Repeat("```x\ny\n```\n譯文\n", 200)},
		{"```x\n" + strings.Repeat("正文", 2000) + "\n```", "```x\n" + strings.Repeat("正文", 2000) + "\n```"},
		{"```x\n不同\n```", "```x\n變化\n```"},
		{strings.Repeat("~~~x\ny\n~~~\n原文\n", 200), strings.Repeat("~~~x\ny\n~~~\n譯文\n", 200)},
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

func TestQQFAQCodeBlockValidationMatchesPreviousForClosedBackticks(t *testing.T) {
	prose := strings.Repeat("合成答案。", 20) + "\n"
	for _, pair := range [][2]string{
		{"```x\na\n```", "```x\na\n```"}, {"```x\na\n```", "```x\nb\n```"},
		{"```x\r\na\r\n```", "```x\na\n```"}, {"```x\na\n```", ""},
		{"```行内代码```", "普通译文"},
		{strings.Repeat("```x\ny\n```\n", 200), strings.Repeat("```x\ny\n```\n譯文\n", 200)},
	} {
		entry := faqValidationEntry(prose + pair[0])
		entry.AnswerTW = prose + pair[1]
		s := faqValidationServer(t, entry)
		got, gotErr := s.qqSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		want, wantErr := s.previousQQSummariseFAQ(t.Context(), "合成问答", []string{"合成分类"})
		if got != want || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Fatalf("普通闭合围栏校验变化：%v，期望 %v", gotErr, wantErr)
		}
	}
}

func TestQQFAQCodeBlockValidationAllocationBudget(t *testing.T) {
	answer := strings.Repeat("合成答案。", 20) + "\n" + strings.Repeat("```\nx\n```\n", 200)
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
		{"ManyBlocks", strings.Repeat("合成答案。", 20) + "\n" + strings.Repeat("```\nx\n```\n", 200)},
		{"LongBlock", strings.Repeat("合成答案。", 20) + "\n```text\n" + strings.Repeat("x", 2000) + "\n```"},
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
		{"ManyBlocks", strings.Repeat("```x\ny\n```\n原文\n", 200), strings.Repeat("```x\ny\n```\n譯文\n", 200)},
		{"LongBlock", "```text\n" + strings.Repeat("x", 2000) + "\n```", "```text\n" + strings.Repeat("x", 2000) + "\n```"},
		{"FirstMismatch", "```text\na\n```\n" + strings.Repeat("```x\ny\n```\n", 200), "```text\nb\n```\n" + strings.Repeat("```x\ny\n```\n", 200)},
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
