package server

import (
	"math/rand"
	"strings"
	"testing"
)

func previousQQStripBadAnswerFiller(text string) string {
	return qqBadAnswerFiller.ReplaceAllString(text, "")
}

func TestQQBadAnswerFillerScanMatchesPrevious(t *testing.T) {
	check := func(input string) {
		want := previousQQStripBadAnswerFiller(input)
		if got := qqStripBadAnswerFiller(input); got != want {
			t.Fatalf("差评语气词扫描变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
	for _, input := range []string{
		"", "不对", "好像还是不行", "你的回答不对！", "答案：答错了", "候选😀", "甲\xff乙\xfe",
		"\u00a0\u2003\u3000", "a.b+c", "这个这个还是好像感觉你说的你说得你这个你的回答答案还你的",
	} {
		check(input)
	}
	random := rand.New(rand.NewSource(1044))
	units := []string{"", "还是", "好像", "感觉", "你说的", "你说得", "你这个", "你的", "这个", "回答", "答案", "还", "你", "的", "啊", "。", "！", "—", " ", "\t", "\n", "\xff", "甲", "😀", "a"}
	for range 10000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		check(text.String())
	}
}

func TestQQBadAnswerFillerScanAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		maxAllocs   float64
		maxBytes    int64
	}{
		{"plain", strings.Repeat("普通文字", 32768), 0, 0},
		{"dense", strings.Repeat("好像还是你的回答！", 4096), 1, 128 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(20, func() { got = qqStripBadAnswerFiller(tc.input) })
			if got != previousQQStripBadAnswerFiller(tc.input) {
				t.Fatal("语气词扫描结果变化")
			}
			if !raceEnabled && allocs > tc.maxAllocs {
				t.Fatalf("语气词扫描分配 %.1f 次，超过 %.1f 次预算", allocs, tc.maxAllocs)
			}
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = qqStripBadAnswerFiller(tc.input)
				}
			})
			if result.AllocedBytesPerOp() > tc.maxBytes {
				t.Fatalf("语气词扫描分配 %d 字节，超过 %d 字节预算", result.AllocedBytesPerOp(), tc.maxBytes)
			}
		})
	}
}

func BenchmarkQQBadAnswerFillerScan(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", strings.Repeat("普通回答。", 4096)},
		{"dense", strings.Repeat("好像还是你的回答！", 1024)},
	} {
		for _, implementation := range []struct {
			name string
			run  func(string) string
		}{
			{"previous", previousQQStripBadAnswerFiller},
			{"current", qqStripBadAnswerFiller},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.run(tc.input)
				}
			})
		}
	}
}

func TestQQBadAnswerQuestionCandidateMatchesPattern(t *testing.T) {
	cases := []string{"", "删除键怎么用", "如何设置", "为什么不行", "为啥", "有吗", "真的？", "really?", "普通反馈", "不准了", "\xff怎么"}
	for _, text := range cases {
		want := qqBadAnswerQuestion.MatchString(text)
		if got := qqBadAnswerQuestionCandidate(text); got != want {
			t.Fatalf("差评问句预筛变化：输入=%q，结果=%v，期望=%v", text, got, want)
		}
	}
}

func TestQQBadAnswerQuestionCandidateRandomCompatibility(t *testing.T) {
	random := rand.New(rand.NewSource(20261020))
	units := []string{"普通", "怎么", "如何", "为什么", "为啥", "吗", "？", "?", "不", "\xff", "\xe4", "\x80"}
	for range 3000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		if got, want := qqBadAnswerQuestionCandidate(input), qqBadAnswerQuestion.MatchString(input); got != want {
			t.Fatalf("随机差评问句预筛不兼容：输入=%q，结果=%v，期望=%v", input, got, want)
		}
	}
}

func BenchmarkQQBadAnswerQuestionCandidate(b *testing.B) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"missing", strings.Repeat("普通反馈。", 32)},
		{"early", "怎么用" + strings.Repeat("普通反馈。", 32)},
		{"late", strings.Repeat("普通反馈。", 32) + "为什么"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string) bool
		}{
			{"previous", qqBadAnswerQuestion.MatchString}, {"current", qqBadAnswerQuestionCandidate},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.fn(tc.text)
				}
			})
		}
	}
}

func previousQQBadAnswerNegative(text string) bool {
	return qqBadAnswerPattern.MatchString(text) || qqBadAnswerLead.MatchString(text)
}

func TestQQBadAnswerNegativeMatchesPatterns(t *testing.T) {
	cases := []string{"不对", "不对了", "不对啊", "不对，我是安卓", "答错了：原因", "没解决 ", "不是这样。", "看不懂", "删除键不对", "不对？", "答非所问x", "普通"}
	for _, text := range cases {
		want := previousQQBadAnswerNegative(text)
		if got := qqBadAnswerNegative(text); got != want {
			t.Fatalf("差评否定词匹配变化：输入=%q，结果=%v，期望=%v", text, got, want)
		}
	}
}

func TestQQBadAnswerNegativeRandomCompatibility(t *testing.T) {
	random := rand.New(rand.NewSource(20261021))
	units := []string{"不对", "答错", "错了", "没用", "不准", "不是这样", "胡说", "瞎说", "乱说", "不行", "没解决", "答非所问", "看不懂", "了", "啊", "，", " ", "x", "普通", "\xff"}
	for range 3000 {
		var text strings.Builder
		for range random.Intn(60) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		if got, want := qqBadAnswerNegative(input), previousQQBadAnswerNegative(input); got != want {
			t.Fatalf("随机差评否定词不兼容：输入=%q，结果=%v，期望=%v", input, got, want)
		}
	}
}

func BenchmarkQQBadAnswerNegative(b *testing.B) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"plain", "普通反馈内容"},
		{"exact", "不对了"},
		{"lead", "答错了，我是安卓"},
		{"late", strings.Repeat("普通反馈", 8) + "不准"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string) bool
		}{
			{"previous", previousQQBadAnswerNegative}, {"current", qqBadAnswerNegative},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.fn(tc.text)
				}
			})
		}
	}
}
