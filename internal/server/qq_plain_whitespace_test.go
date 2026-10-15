package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQPlainTextWhitespaceKeepsRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"ascii_runs", "甲 \t \t乙", "甲 乙"},
		{"markers", "  > \t -\t \t**甲**\t\t乙 \n 1.\t \t丙", "甲 乙 丙"},
		{"bare_link", "甲 https://example.com \t \t乙", "甲 乙"},
		{"multiline_link", "甲\t\t[乙\n丙](https://example.com)\t\t丁", "甲 乙 丙 丁"},
		{"trimmed_marker", "- \t \n正文", "- 正文"},
		{"quoted_table", " \t |甲| \n > \t |乙| \t \t丙", "|乙| 丙"},
		{"unicode_marker_space", "#\u00a0甲\t \t乙", "# 甲 乙"},
		{"empty", "\t \n\u3000 \t", ""},
		{"invalid_utf8", "甲\xff \t乙\xfe", "甲\xff 乙\xfe"},
		{"fence", "甲\t\t乙\n```\n忽略\t\t内容\n```\n丙", "甲 乙 丙"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqPlainText(tc.input); got != tc.want {
				t.Fatalf("空白处理结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQPlainTextWhitespaceAllocationBudget(t *testing.T) {
	word := strings.Repeat("水", 32768)
	for _, tc := range []struct {
		name, input, want string
		budget            uint64
	}{
		{"mixed_runs", strings.Repeat("词 \t \t", 32768), strings.TrimSpace(strings.Repeat("词 ", 32768)), 192 << 10},
		{"large_gap", word + strings.Repeat(" \t", 32768) + word, word + " " + word, 256 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqPlainText(tc.input)
			runtime.ReadMemStats(&after)
			if got != tc.want {
				t.Fatal("长正文的空白处理结果不正确")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("空白处理分配 %d 字节", allocated)
			// 输入和预期在测量前构造，预算允许最终正文及运行时开销，排除空白归一化的中间副本。
			if allocated > tc.budget {
				t.Fatalf("空白处理分配 %d 字节，超过 %d 字节预算", allocated, tc.budget)
			}
		})
	}
}

func BenchmarkQQPlainTextWhitespace(b *testing.B) {
	word := strings.Repeat("水", 32768)
	for _, tc := range []struct{ name, input string }{
		{"mixed_runs", strings.Repeat("词 \t \t", 32768)},
		{"large_gap", word + strings.Repeat(" \t", 32768) + word},
		{"padding", strings.Repeat(" \t", 32768) + "正文" + strings.Repeat("\t ", 32768)},
		{"formatted_runs", strings.Repeat("> - **词** \t `词`\n", 1024)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(tc.input)
			}
		})
	}
}
