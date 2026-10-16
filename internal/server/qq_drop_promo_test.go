package server

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQQPromoSentenceCandidate(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"普通回答。", false},
		{"加群 12345678", true},
		{"微信号 abc12", true},
		{"visit v.example", true},
	} {
		if got := qqPromoSentenceCandidate(tc.input); got != tc.want {
			t.Errorf("qqPromoSentenceCandidate(%q) = %v，预期 %v", tc.input, got, tc.want)
		}
	}
}

func TestQQDropPromoSentencesKeepsRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "普通回答。", "普通回答。"},
		{"lines", "第一行\n第二行", "第一行\n第二行"},
		{"promo", "普通回答。加群 12345678 下载。后续说明。", "普通回答。后续说明。"},
		{"all_promo_line", "加群 12345678。\n保留。", "保留。"},
		{"empty", "", ""},
		{"invalid_utf8", "甲\xff。乙\xfe", "甲\xff。乙\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqDropPromoSentences(tc.input); got != tc.want {
				t.Fatalf("引流句过滤结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQDropPromoSentencesNoopAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"long_plain", strings.Repeat("普通回答。", 8192)},
		{"long_lines", strings.Repeat("普通回答。\n", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqDropPromoSentences(tc.input)
			runtime.ReadMemStats(&after)
			if got != strings.TrimSpace(tc.input) {
				t.Fatal("无引流句正文发生变化")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("引流句过滤分配 %d 字节", allocated)
			// 输入在测量前构造；无匹配时应复用正文，预算允许运行时开销。
			if allocated > 64<<10 {
				t.Fatalf("引流句过滤分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func BenchmarkQQDropPromoSentences(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short_plain", "普通回答。"},
		{"long_plain", strings.Repeat("普通回答。", 8192)},
		{"long_lines", strings.Repeat("普通回答。\n", 8192)},
		{"promo", strings.Repeat("普通回答。", 1024) + "加群 12345678。后续说明。"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqDropPromoSentences(tc.input)
			}
		})
	}
}

func TestQQDropPromoSentencesFilteredAllocationBudget(t *testing.T) {
	input := strings.Repeat("普通回答。", 1024) + "加群 12345678。后续说明。"
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_ = qqDropPromoSentences(input)
		}
	})
	if !raceEnabled && result.AllocsPerOp() > 3 {
		allocated := result.AllocsPerOp()
		t.Fatalf("引流句过滤分配 %d 次，超过 3 次预算", allocated)
	}
}

func TestQQDropPromoSentencesMatchesPrevious(t *testing.T) {
	previous := func(answer string) string {
		if !qqPromoSentenceCandidate(answer) {
			return strings.TrimSpace(answer)
		}
		lines := strings.Split(answer, "\n")
		kept := lines[:0]
		for _, line := range lines {
			var b strings.Builder
			dropped := false
			for rest := line; rest != ""; {
				end := len(rest)
				if i := strings.IndexAny(rest, qqSentenceEnds); i >= 0 {
					_, size := utf8.DecodeRuneInString(rest[i:])
					end = i + size
				}
				sentence := rest[:end]
				rest = rest[end:]
				if qqPromoSentence(sentence) != "" {
					dropped = true
					continue
				}
				b.WriteString(sentence)
			}
			if dropped && strings.TrimSpace(b.String()) == "" {
				continue
			}
			kept = append(kept, b.String())
		}
		return strings.TrimSpace(strings.Join(kept, "\n"))
	}
	random := rand.New(rand.NewSource(21))
	units := []string{"普通", "。", "加群 12345", "微信号 abc12", "v.example", "\n", " ", "\xff"}
	for range 1000 {
		var input strings.Builder
		for range random.Intn(80) {
			input.WriteString(units[random.Intn(len(units))])
		}
		if got, want := qqDropPromoSentences(input.String()), previous(input.String()); got != want {
			t.Fatalf("引流句过滤不兼容：输入=%q，结果=%q，预期=%q", input.String(), got, want)
		}
	}
}
