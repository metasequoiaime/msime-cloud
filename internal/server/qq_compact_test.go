package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQCompactConditionalRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "候选框在哪里", "候选框在哪里"},
		{"spaces", "候选 框 在哪 里", "候选框在哪里"},
		{"case", "Help", "help"},
		{"slash", "/help", "help"},
		{"fullwidth", "帮助？", "帮助"},
		{"trailing_emoji", "帮助🙏", "帮助"},
		{"question_mark", "?", "?"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqCompact(tc.input); got != tc.want {
				t.Fatalf("命令规范化结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQCompactNoopAllocationBudget(t *testing.T) {
	input := strings.Repeat("候选框在哪里", 1024)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := qqCompact(input)
	runtime.ReadMemStats(&after)
	if got != input {
		t.Fatal("普通消息发生变化")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 64<<10 {
		t.Fatalf("普通消息命令规范化分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}

func BenchmarkQQCompact(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short_plain", "候选框在哪里"},
		{"long_plain", strings.Repeat("候选框在哪里", 1024)},
		{"spaces", strings.Repeat("候选 框 在哪 里 ", 1024)},
		{"command", "帮助？"},
		{"emoji", "最新版本🙏"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompact(tc.input)
			}
		})
	}
}
