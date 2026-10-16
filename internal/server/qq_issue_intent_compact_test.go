package server

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func previousQQIssueIntent(text string) bool {
	compact := strings.ToLower(strings.Join(strings.Fields(text), ""))
	return slices.ContainsFunc(qqIssueIntentWords, func(word string) bool { return strings.Contains(compact, word) })
}

func TestQQIssueIntentCompactMatchesPrevious(t *testing.T) {
	texts := []string{
		"", "普通问题", "提 I S S U E", "记\u0085一\u3000下", "提\u0130SSUE", "提 iſſue", "提 ＩＳＳＵＥ",
		"历史记录丢了", "\ufffd 提 ISSUE", "\xff提 ISSUE", "\xe6 \x8f \x90 I S S U E", "提\xffISSUE",
		"提\u200bissue", "提\u0000issue", "\u023a\u212a\u0130提\n个\tISSUE", "\r\n\v\f\t ",
	}
	random := rand.New(rand.NewSource(651))
	units := []string{"普通正文", "提", "建", "开", "记", "帮", "反", "个", "一", "下", "I", "S", "U", "E", "s", "İ", "ſ", "K", "Ⱥ", "\ufffd", "\xff", "\xe6", "\x8f", "\x90", "\r\n", "\v", "\f", " ", "\u0085", "\u00a0", "\u3000", "\x00"}
	for range 3000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(units[random.Intn(len(units))])
		}
		if random.Intn(3) == 0 {
			text.WriteString(qqIssueIntentWords[random.Intn(len(qqIssueIntentWords))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		if got, want := qqCompactLower(text), strings.ToLower(strings.Join(strings.Fields(text), "")); got != want {
			t.Fatalf("意图文本规范化变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
		if got, want := qqIssueIntent(text), previousQQIssueIntent(text); got != want {
			t.Fatalf("意图识别变化：输入=%q，结果=%t，期望=%t", text, got, want)
		}
	}
}

func qqIssueIntentAllocationCases() []struct {
	name, text string
	want       bool
	budget     int64
} {
	return []struct {
		name, text string
		want       bool
		budget     int64
	}{
		{"普通消息", strings.Repeat("普通问题", 10000), false, 0},
		{"无需改写的意图", "提issue" + strings.Repeat("普通正文", 10000), true, 0},
		{"密集空白", strings.Repeat("普通 ", 10000) + "提 I S S U E", true, 2*int64(len(strings.Repeat("普通", 10000))) + (16 << 10)},
		{"空白与大写", strings.Repeat("BODY ", 10000) + "提 ISSUE", true, 2*int64(len(strings.Repeat("body", 10000))) + (16 << 10)},
		{"大量首尾空白", strings.Repeat(" ", 1<<20) + "提 ISSUE" + strings.Repeat("\u3000", 1<<18), true, 4096},
		{"全部空白", strings.Repeat(" \t\u3000", 10000), false, 0},
	}
}

func TestQQIssueIntentCompactAllocationBudget(t *testing.T) {
	for _, tc := range qqIssueIntentAllocationCases() {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqIssueIntent(tc.text)
				}
			})
			if got != tc.want || result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("结果=%t，期望=%t，分配=%s，预算=%d 字节", got, tc.want, result.MemString(), tc.budget)
			}
		})
	}
}

func BenchmarkQQIssueIntentCompact(b *testing.B) {
	for _, implementation := range []struct {
		name string
		run  func(string) bool
	}{
		{"previous", previousQQIssueIntent},
		{"current", qqIssueIntent},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			for _, tc := range qqIssueIntentAllocationCases() {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = implementation.run(tc.text)
					}
				})
			}
		})
	}
}
