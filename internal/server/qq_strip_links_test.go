package server

import (
	"runtime"
	"strings"
	"testing"
)

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
