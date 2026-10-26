package server

import (
	"context"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// 冻结优化前的前缀表达式，差分测试与基准不依赖生产实现。
var previousQQAnswerPrefix = regexp.MustCompile(`^答[:：]\s*`)

func previousQQTrimAnswerPrefix(text string) string {
	return previousQQAnswerPrefix.ReplaceAllString(text, "")
}

func TestQQAnswerPrefixMatchesPrevious(t *testing.T) {
	check := func(input string) {
		t.Helper()
		if got, want := qqTrimAnswerPrefix(input), previousQQTrimAnswerPrefix(input); got != want {
			t.Fatalf("答案前缀裁剪变化：输入=%q，实际=%q，期望=%q", input, got, want)
		}
	}
	for _, prefix := range []string{"", "答:", "答：", "答", "答 :", "答:答:", " 答：", "答\xff:", "答﹕"} {
		for _, padding := range []string{"", " ", "\t\n\r\f ", "\v", "\u0085", "\u00a0", "\u3000", "\xff"} {
			for _, body := range []string{"", "打开设置。", "答：原文", "第一行\n第二行", strings.Repeat("正文", 4096)} {
				check(prefix + padding + body)
			}
		}
	}
	for _, prefix := range []string{"答:", "答："} {
		for b := 0; b < 256; b++ {
			check(prefix + string([]byte{byte(b)}) + "正文")
		}
	}
	rng := rand.New(rand.NewPCG(137, 149))
	units := []string{"答", ":", "：", " ", "\t", "\n", "\r", "\f", "\v", "\u3000", "\u00a0", "\xff", "\xe7", "\xad", "\x94", "正文"}
	for range 4096 {
		var input strings.Builder
		for range rng.IntN(20) {
			input.WriteString(units[rng.IntN(len(units))])
		}
		check(input.String())
	}
}

func TestQQAnswerPrefixWithoutAllocations(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"半角冒号", "答: 打开设置。"},
		{"全角冒号", "答：\t\n打开设置。"},
		{"无前缀", "打开设置。"},
		{"空答案", "答：\r\n"},
		{"保留垂直制表符", "答：\v正文"},
		{"保留全角空格", "答：\u3000正文"},
		{"非法字节", "答：\xff正文"},
		{"长答案", "答： " + strings.Repeat("打开设置。", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(1000, func() { got = qqTrimAnswerPrefix(tc.input) })
			if want := previousQQTrimAnswerPrefix(tc.input); got != want {
				t.Fatalf("答案前缀裁剪变化：实际=%q，期望=%q", got, want)
			}
			if allocs != 0 {
				t.Fatalf("裁剪答案前缀不应分配内存：分配次数=%g", allocs)
			}
		})
	}
}

func TestQQAnswerPrefixCommandContent(t *testing.T) {
	s, fake, _ := qqServer(t)
	candidates, bad := &memoryFAQCandidates{}, &memoryBadAnswers{}
	s.qqFAQCandidates, s.qqBadAnswers = candidates, bad
	ctx := context.Background()
	if _, err := candidates.AddQQFAQCandidate(ctx, "怎样切换方案", "原答案"); err != nil {
		t.Fatal(err)
	}
	if err := bad.AddQQBadAnswer(ctx, "怎样切换方案", "原答案", "不对", "原答案"); err != nil {
		t.Fatal(err)
	}
	fake.faqFiles = map[string]string{"guides/faq.md": qqTestFAQ}
	fake.faq = `{"valid":"no"}`
	for _, code := range []string{"F1", "B1"} {
		for _, tc := range []struct{ input, answer string }{
			{"答: \t打开设置。", "打开设置。"},
			{"答：\n\u3000打开设置。\u00a0", "打开设置。"},
			{"直接打开设置。", "直接打开设置。"},
			{"答：答：这部分保留。", "答：这部分保留。"},
			{"答：\t\u3000", "原答案"},
		} {
			before := len(fake.chatBodies)
			command := "写进FAQ " + code + " " + tc.input
			s.qqFAQCandidateCommands(ctx, command, qqCompact(command), true, func(string) {})
			if len(fake.chatBodies) != before+1 {
				t.Fatalf("命令未交给答案整理：编号=%s，输入=%q", code, tc.input)
			}
			if got, want := fake.chatBodies[before], "问：怎样切换方案\n答："+tc.answer; got != want {
				t.Fatalf("交给答案整理的正文变化：编号=%s，实际=%q，期望=%q", code, got, want)
			}
		}
	}
}

func BenchmarkQQAnswerPrefix(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"短答案", "答： 打开设置。"},
		{"长答案", "答： " + strings.Repeat("打开设置。", 4096)},
		{"无前缀长答案", strings.Repeat("打开设置。", 4096)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) string
		}{{"previous", previousQQTrimAnswerPrefix}, {"current", qqTrimAnswerPrefix}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input)
				}
			})
		}
	}
}

func BenchmarkQQAnswerPrefixCommand(b *testing.B) {
	s := &Server{}
	for _, tc := range []struct{ name, body string }{
		{"短答案", "打开设置。"},
		{"长答案", strings.Repeat("打开设置。", 4096)},
	} {
		text := "写进FAQ F1 答： " + tc.body
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = s.qqFAQCandidateCommands(context.Background(), text, "", false, func(string) {})
			}
		})
	}
}
