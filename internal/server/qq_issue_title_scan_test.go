package server

import (
	"math/rand"
	"strings"
	"testing"
)

func previousQQTrimIssueTitleTag(text string) string {
	return qqIssueTitleTag.ReplaceAllString(text, "")
}

func TestQQTrimIssueTitleTagMatchesPrevious(t *testing.T) {
	check := func(input string) {
		if got, want := qqTrimIssueTitleTag(input), previousQQTrimIssueTitleTag(input); got != want {
			t.Fatalf("issue 标题前缀扫描变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
	for _, input := range []string{
		"", "标题", "[Bug] 标题", " [Feature]: 标题", "\n\t[问题]：标题", "[] 标题",
		"[甲乙丙]标题", "[😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀😀]标题",
		"[123456789012345678901]标题", "[标题\n换行] 标题", "[标题\xff]标题",
		"[Bug]", "[Bug]   ", "[Bug]:   ", "[[Bug] 标题", "[Bug 标题",
	} {
		check(input)
	}
	random := rand.New(rand.NewSource(1045))
	units := []string{"", " ", "\t", "\n", "\r", "\f", "[", "]", ":", "Bug", "问题", "😀", "\xff", "标题"}
	for range 10000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		check(text.String())
	}
}

func TestQQTrimIssueTitleTagAllocationBudget(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("普通标题。", 32768),
		"[Bug] " + strings.Repeat("普通标题。", 32768),
	} {
		var got string
		allocs := testing.AllocsPerRun(20, func() { got = qqTrimIssueTitleTag(input) })
		if got != previousQQTrimIssueTitleTag(input) {
			t.Fatal("issue 标题清理结果变化")
		}
		if allocs != 0 {
			t.Fatalf("issue 标题清理分配 %.1f 次，预期零分配", allocs)
		}
	}
}

func BenchmarkQQTrimIssueTitleTag(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", strings.Repeat("普通标题。", 4096)},
		{"tag", "[Bug] " + strings.Repeat("普通标题。", 4096)},
	} {
		for _, implementation := range []struct {
			name string
			run  func(string) string
		}{
			{"previous", previousQQTrimIssueTitleTag},
			{"current", qqTrimIssueTitleTag},
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
