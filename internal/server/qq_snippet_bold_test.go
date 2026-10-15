package server

import (
	"fmt"
	"math/rand"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

var qqPreviousBoldPattern = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)

func qqPreviousBoldSnippet(text string) string {
	var words []string
	for _, match := range qqPreviousBoldPattern.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(words, match[1]) {
			words = append(words, match[1])
		}
	}
	if len(words) > 0 {
		return qqTruncate(strings.Join(words, "、"), qqSnippetRunes)
	}
	if labels := qqListLabels(text); len(labels) >= 2 {
		return qqTruncate(strings.Join(labels, "、"), qqSnippetRunes)
	}
	return qqTruncate(strings.Join(strings.Fields(qqMarkdownNoise.ReplaceAllString(text, " ")), " "), qqSnippetRunes)
}

func TestQQSnippetBoldKeepsPreviousOutput(t *testing.T) {
	texts := []string{
		"", "普通正文", "**水杉**", "***水杉**", "****水杉****", "**甲****乙**", "**甲*乙**丙**",
		"**甲\n乙**", "**甲\r乙**", "**甲\xff乙**", "**\xe4\xb8**", "** \t\u3000 **",
		"- 甲：说明\n- 乙：说明\n正文**重点**", "- 甲：说明\n- 乙：说明\n**未闭合",
		strings.Repeat("*", 65536), "**" + strings.Repeat("正文", 32768) + "*尾部",
	}
	for _, count := range []int{qqSnippetRunes - 1, qqSnippetRunes, qqSnippetRunes + 1} {
		word := strings.Repeat("水", count)
		for _, tail := range []string{"", "**" + word + "**", "**乙**", "**\xff**", "普通尾部"} {
			texts = append(texts, "**"+word+"**"+tail)
		}
	}
	random := rand.New(rand.NewSource(37))
	units := []string{"水", "😀", "a", " ", "\t", "\n", "\r", "\u3000", "\xff", "\xe4\xb8", "*", "**", "***", "#", "_", "[", "](", ")", "|"}
	for range 1000 {
		var input strings.Builder
		for range 100 {
			input.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, input.String())
	}
	for _, input := range texts {
		if got, want := qqSnippet(input), qqPreviousBoldSnippet(input); got != want {
			t.Fatalf("加粗摘要不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQSnippetBoldUsesBoundedMemory(t *testing.T) {
	input := strings.Repeat("**水杉** ", 32768)
	// 先预热匹配器，避免把初始化内存计入摘要预算。
	if got := qqSnippet("**水杉**"); got != "水杉" {
		t.Fatalf("加粗摘要=%q，预期水杉", got)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := qqSnippet(input)
	runtime.ReadMemStats(&after)
	if got != "水杉" {
		t.Fatalf("重复加粗词摘要=%q，预期水杉", got)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<10 {
		t.Fatalf("加粗摘要分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}

func BenchmarkQQSnippetBold(b *testing.B) {
	var distinct strings.Builder
	for i := range 8192 {
		fmt.Fprintf(&distinct, "**词%04d** ", i)
	}
	for _, tc := range []struct{ name, input string }{
		{"short", "**换肤**、**删除**、**隐私**"},
		{"distinct", distinct.String()},
		{"repeated", strings.Repeat("**水杉** ", 32768)},
		{"long_word", "**" + strings.Repeat("水", 65536) + "**"},
		{"star_run", strings.Repeat("*", 1<<18)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSnippet(tc.input)
			}
		})
	}
}
