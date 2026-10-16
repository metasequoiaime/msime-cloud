package server

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func qqPreviousListSnippet(text string) string {
	var labels []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "- ")
		if !ok {
			if rest, ok = strings.CutPrefix(line, "* "); !ok {
				continue
			}
		}
		runes := []rune(rest)
		i := slices.IndexFunc(runes, func(r rune) bool { return r == '：' || r == ':' })
		if i < 0 || i >= qqListLabelRunes || (i+1 < len(runes) && runes[i+1] == '/') {
			continue
		}
		label := strings.Join(strings.Fields(qqMarkdownNoise.ReplaceAllString(string(runes[:i]), " ")), " ")
		if label != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	if len(labels) >= 2 {
		return qqTruncate(strings.Join(labels, "、"), qqSnippetRunes)
	}
	return qqTruncate(strings.Join(strings.Fields(qqMarkdownNoise.ReplaceAllString(text, " ")), " "), qqSnippetRunes)
}

func TestQQSnippetLabelsKeepsPreviousOutput(t *testing.T) {
	texts := []string{
		"", "普通正文", "- 换肤：说明", "- 换肤：说明\n- 换肤：重复",
		"- 换肤：说明\n- 删除：说明\n- 换肤：重复\n- 隐私：说明",
		"- 水\xff：说明\n- 杉\xfe：说明", "- ：说明\n- https://example.test\n- 水：说明",
		strings.Repeat("- 甲：重复\n", 8192) + "- 乙：末尾",
	}
	for _, count := range []int{qqSnippetRunes - 1, qqSnippetRunes, qqSnippetRunes + 1} {
		var input strings.Builder
		for i := range count {
			fmt.Fprintf(&input, "- %c：说明\n", rune(0x4e00+i))
		}
		texts = append(texts, input.String())
	}
	// 四个标签连同分隔符恰好 47 字，后续标签覆盖 49、50、51 字边界。
	prefix := "- 一一一一一一一一一一一：说明\n- 二二二二二二二二二二二：说明\n- 三三三三三三三三三三三：说明\n- 四四四四四四四四四四四：说明\n"
	for _, tail := range []string{"甲", "甲乙", "甲乙丙", "甲乙丙丁"} {
		texts = append(texts, prefix+"- "+tail+"：尾部")
	}
	exact := prefix + "- 甲乙：恰好 50 字\n"
	texts = append(texts, exact+"- 甲乙：重复", exact+"- 甲乙：重复\n- 乙：末尾")
	random := rand.New(rand.NewSource(41))
	units := []string{"水", "😀", "a", " ", "\t", "\u3000", "\xff", "\xe4\xb8", "`", "#", "_", "[", "](", ")", "|", ":", "：", "/"}
	for range 200 {
		var input strings.Builder
		for range 40 {
			input.WriteString("- ")
			for range random.Intn(15) {
				input.WriteString(units[random.Intn(len(units))])
			}
			input.WriteString("：说明\n")
		}
		texts = append(texts, input.String())
	}
	for _, input := range texts {
		if got, want := qqSnippet(input), qqPreviousListSnippet(input); got != want {
			t.Fatalf("列表摘要不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQSnippetLabelsUsesBoundedCollectionMemory(t *testing.T) {
	var input strings.Builder
	for i := range 8192 {
		fmt.Fprintf(&input, "- 词%04d：说明\n", i)
	}
	text := input.String()
	want := qqPreviousListSnippet(text)
	var got string
	// 按多次调用的均值计量，避免同进程后台任务的偶发分配污染单次全局读数。
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqSnippet(text)
		}
	})
	if got != want {
		t.Fatalf("列表摘要=%q，预期 %q", got, want)
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 256<<10 {
		t.Fatalf("列表摘要分配 %d 字节，超过 256 KiB 预算", allocated)
	}
}

func BenchmarkQQSnippetLabels(b *testing.B) {
	var distinct strings.Builder
	for i := range 8192 {
		fmt.Fprintf(&distinct, "- 词%04d：说明\n", i)
	}
	for _, tc := range []struct{ name, input string }{
		{"short", "- 换肤：说明\n- 删除：说明\n- 隐私：说明"},
		{"distinct", distinct.String()},
		{"repeated", strings.Repeat("- 换肤：说明\n", 32768)},
		{"normalized_duplicates", strings.Repeat("- 换肤：说明\n- `换肤`：说明\n- [换肤](x):说明\n- _换肤_：说明\n", 8192)},
		{"late_second", strings.Repeat("- 换肤：说明\n", 32768) + "- 隐私：尾部"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSnippet(tc.input)
			}
		})
	}
}
