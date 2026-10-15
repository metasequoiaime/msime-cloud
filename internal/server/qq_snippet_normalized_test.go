package server

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

func BenchmarkQQSnippetNormalized(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short", "普通正文"},
		{"many_words", strings.Repeat("水杉 ", 32768)},
		{"long_field", strings.Repeat("水", 65536)},
		{"markdown_noise", strings.Repeat("水杉 `代码` ", 32768)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSnippet(tc.input)
			}
		})
	}
}

func TestQQSnippetNormalizedUsesBoundedMemory(t *testing.T) {
	input := strings.Repeat("水杉 ", 32768)
	want := qqTruncate(strings.Join(strings.Fields(input), " "), qqSnippetRunes)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := qqSnippet(input)
	runtime.ReadMemStats(&after)
	if got != want {
		t.Fatalf("摘要结果不正确：结果=%q，预期=%q", got, want)
	}
	// 最终摘要只有 50 字，不应为整段正文创建字段切片和拼接缓冲。
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<10 {
		t.Fatalf("摘要分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}

func TestQQSnippetNormalizedKeepsPreviousOutput(t *testing.T) {
	texts := []string{"", " \t\u3000", "甲\xff 乙\xfe", "甲\x00乙", "\u200b甲\u0301乙", "[链接](https://example.test) 尾部"}
	for _, count := range []int{qqSnippetRunes - 1, qqSnippetRunes, qqSnippetRunes + 1} {
		for _, tail := range []string{"", " \u3000\n", " 乙", "\xff", "\xe4\xb8"} {
			texts = append(texts, strings.Repeat("水", count)+tail)
		}
	}
	random := rand.New(rand.NewSource(23))
	units := []string{"水", "😀", "a", " ", "\t", "\r", "\n", "\v", "\f", "\u3000", "\u0085", "\xff", "\xe4\xb8", "`", "#", "_", "[", "|"}
	for range 200 {
		var input strings.Builder
		for range 100 {
			input.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, input.String())
	}
	for _, input := range texts {
		want := qqTruncate(strings.Join(strings.Fields(qqMarkdownNoise.ReplaceAllString(input, " ")), " "), qqSnippetRunes)
		if got := qqSnippet(input); got != want {
			t.Fatalf("摘要规范化不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQSnippetNormalizedKeepsLateSummarySelection(t *testing.T) {
	prefix := strings.Repeat("水杉 ", 32768)
	for _, tc := range []struct{ input, want string }{
		{prefix + "**重点**", "重点"},
		{prefix + "\n- 甲：说明\n- 乙：说明", "甲、乙"},
		{prefix + "\n- 甲：说明\n- 乙：说明\n**重点**", "重点"},
	} {
		if got := qqSnippet(tc.input); got != tc.want {
			t.Fatalf("正文后部摘要选择不兼容：结果=%q，预期=%q", got, tc.want)
		}
	}
}
