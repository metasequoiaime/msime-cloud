package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// 固定优化前的标题检测，只改函数名，供语义和分配开销对照。
func previousQQFAQAnswerHasHeadingBeforeCandidate(answer string) bool {
	// ATX 和 Setext 标题必含这些字符，普通答案无需构建解析树。
	if !strings.ContainsAny(answer, "#=-") {
		return false
	}
	root := qqFAQAnswerParser.Parse(text.NewReader([]byte(answer)))
	found := false
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if heading, ok := node.(*ast.Heading); entering && ok && heading.Level <= 3 {
			found = true
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found
}

func qqFAQHeadingCandidateSamples() []struct{ name, answer string } {
	return []struct{ name, answer string }{
		{"ListOptions", strings.Repeat("- 使用 `tool --option=value` 完成设置。\n", 60)},
		{"Assignments", strings.Repeat("参数 key=value 用于设置。\n", 100)},
		{"HyphenatedProse", strings.Repeat("使用 msime-cloud 命令完成设置。\n", 100)},
	}
}

func TestQQFAQHeadingCandidateAllocationBudget(t *testing.T) {
	for _, tc := range qqFAQHeadingCandidateSamples() {
		t.Run(tc.name, func(t *testing.T) {
			if utf8.RuneCountInString(tc.answer) > qqFAQRunes {
				t.Fatal("样本超过 FAQ 字数限制")
			}
			var got, previous bool
			old := testing.AllocsPerRun(20, func() { previous = previousQQFAQAnswerHasHeadingBeforeCandidate(tc.answer) })
			current := testing.AllocsPerRun(20, func() { got = qqFAQAnswerHasHeading(tc.answer) })
			if got || previous {
				t.Fatal("普通列表或参数正文被判断为标题")
			}
			t.Logf("标题检测分配：旧版=%g，当前=%g", old, current)
			if current != 0 {
				t.Fatalf("不含标题候选的答案分配 %g 次，期望零分配", current)
			}
		})
	}
}

func TestQQFAQHeadingCandidateCompatibility(t *testing.T) {
	check := func(answer string) {
		t.Helper()
		if got, want := qqFAQAnswerHasHeading(answer), previousQQFAQAnswerHasHeadingBeforeCandidate(answer); got != want {
			t.Fatalf("标题检测变化：%q，当前=%t，旧版=%t", answer, got, want)
		}
	}
	for _, prefix := range []string{"", " ", "   ", "    ", "\t", "> ", "> > ", "- ", "1. ", "> - ", "- > "} {
		for _, marker := range []string{"#", "##", "###", "####", "#######", "=", "===", "-", "---", "- - -", "\\---"} {
			for _, space := range []string{"", " ", "\t", "\v", "\f", "\x00", "\x85", "\u3000"} {
				for _, newline := range []string{"\n", "\r\n", "\r"} {
					line := prefix + marker + space
					check(line)
					check(line + "合成标题")
					check("普通正文" + newline + line + newline + "尾部正文")
					check("> 普通正文" + newline + line)
					check("- 普通正文" + newline + line)
				}
			}
		}
	}
	for _, answer := range []string{
		"```text\n标题\n---\n```", "<pre>\n标题\n===\n</pre>", "    标题\n    ---", "标题\n--- \t\r\n尾部",
		"标题\n> ---", "- 标题\n  ===", "> 标题\n> ===", "> > 标题\n> > ---", "标题\n---\v", "标题\n===\f",
	} {
		check(answer)
	}
	random := rand.New(rand.NewSource(1533))
	units := []string{"普通", "#", "##", "###", "=", "-", ">", "*", "1.", " ", "\t", "\n", "\r", "\v", "\f", "`", "~", "<div>", "\x00", "\xff"}
	for range 2000 {
		var answer strings.Builder
		for range random.Intn(80) {
			answer.WriteString(units[random.Intn(len(units))])
		}
		check(answer.String())
	}
}

func FuzzQQFAQHeadingCandidate(f *testing.F) {
	for _, s := range []string{"- 普通列表", "参数 key=value", "标题\n=== \t", "> 标题\n> ---", "# 标题", "标题\r---\r尾部", "标题\n===\v\x00\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, answer string) {
		if len(answer) > 4096 {
			t.Skip()
		}
		if got, want := qqFAQAnswerHasHeading(answer), previousQQFAQAnswerHasHeadingBeforeCandidate(answer); got != want {
			t.Fatalf("标题检测变化：%q，当前=%t，旧版=%t", answer, got, want)
		}
	})
}

var qqFAQHeadingCandidateResult bool

func BenchmarkQQFAQHeadingCandidate(b *testing.B) {
	samples := append(qqFAQHeadingCandidateSamples(), struct{ name, answer string }{"ActualHeading", strings.Repeat("普通答案。", 20) + "\n\n> 标题\n> ---"}, struct{ name, answer string }{"PlainProse", strings.Repeat("普通答案。", 100)})
	for _, tc := range samples {
		for _, impl := range []struct {
			name       string
			hasHeading func(string) bool
		}{{"Previous", previousQQFAQAnswerHasHeadingBeforeCandidate}, {"Current", qqFAQAnswerHasHeading}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqFAQHeadingCandidateResult = impl.hasHeading(tc.answer)
				}
			})
		}
	}
}
