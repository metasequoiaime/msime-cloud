package server

import (
	"math/rand"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestQQStripLinksMatchesPattern(t *testing.T) {
	random := rand.New(rand.NewSource(11))
	units := []string{"", "a", "1", "+", "-", ".", ":", "/", "!", " ", "甲", "\xff"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		want := input
		if strings.Contains(input, "://") {
			want = qqLinkPattern.ReplaceAllString(want, "")
		}
		want = strings.TrimSpace(qqCollapseSpaceRuns(want))
		if got := qqStripLinks(input); got != want {
			t.Fatalf("链接过滤不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQStripLinksConditionalRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "普通回答。", "普通回答。"},
		{"trim", "  普通回答。  ", "普通回答。"},
		{"single_spaces", "甲 乙 丙", "甲 乙 丙"},
		{"tab", "甲\t乙", "甲\t乙"},
		{"double_spaces", "甲  乙", "甲 乙"},
		{"link", "甲 https://example.com 乙", "甲 乙"},
		{"invalid_utf8", "甲\xff 乙\xfe", "甲\xff 乙\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqStripLinks(tc.input); got != tc.want {
				t.Fatalf("链接过滤结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQStripLinksNoopAllocationBudget(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("普通回答。", 8192),
		strings.Repeat("普通回答。 ", 8192),
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := qqStripLinks(input)
		runtime.ReadMemStats(&after)
		if got != strings.TrimSpace(input) {
			t.Fatal("无链接正文发生变化")
		}
		allocated := after.TotalAlloc - before.TotalAlloc
		if allocated > 64<<10 {
			t.Fatalf("无链接过滤分配 %d 字节，超过 64 KiB 预算", allocated)
		}
	}
}

func BenchmarkQQStripLinks(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short_plain", "普通回答。"},
		{"long_plain", strings.Repeat("普通回答。", 8192)},
		{"long_single_spaces", strings.Repeat("普通回答。 ", 8192)},
		{"double_spaces", strings.Repeat("普通  ", 8192)},
		{"links", strings.Repeat("普通回答。 ", 1024) + "https://example.com 结尾"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqStripLinks(tc.input)
			}
		})
	}
}

func TestQQStripLinksSpaceRunAllocationBudget(t *testing.T) {
	input := strings.Repeat("普通  ", 8192)
	want := strings.TrimSpace(strings.Repeat("普通 ", 8192))
	var got string
	allocations := testing.AllocsPerRun(20, func() {
		got = qqStripLinks(input)
	})
	if got != want {
		t.Fatal("连续空格合并结果不正确")
	}
	// 只允许构建一个输出缓冲，排除正则替换的反复扩容和中间副本。
	if allocations > 1 {
		t.Fatalf("连续空格合并分配 %.1f 次，预期最多 1 次", allocations)
	}
}

func TestQQStripLinksSpaceRunsMatchPreviousPattern(t *testing.T) {
	previous := regexp.MustCompile(`[ \t]{2,}`)
	units := []string{"", " ", "  ", "\t", "\t\t", " \t ", "\n", "\r", "\v", "\f", "\u3000", "\u0085", "甲", "\xff", "https://example.test", "7://"}
	for _, first := range units {
		for _, middle := range units {
			for _, last := range units {
				input := first + middle + last
				want := strings.TrimSpace(previous.ReplaceAllString(qqLinkPattern.ReplaceAllString(input, ""), " "))
				if got := qqStripLinks(input); got != want {
					t.Fatalf("连续空白处理不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
				}
			}
		}
	}
}

func TestQQStripLinksSpaceRunKeepsSmallOutputBuffer(t *testing.T) {
	input := "甲" + strings.Repeat(" \t", 1<<18) + "乙"
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := qqStripLinks(input)
	runtime.ReadMemStats(&after)
	if got != "甲 乙" {
		t.Fatalf("长空白段处理结果不正确：%q", got)
	}
	// 结果很短时，不应为原输入长度分配并保留缓冲。
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<10 {
		t.Fatalf("长空白段处理分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}
