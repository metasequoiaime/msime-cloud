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
			if allocs > tc.maxAllocs {
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
