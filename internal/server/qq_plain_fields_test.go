package server

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestQQPlainTextFieldOutputKeepsRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"line_boundary", "甲\n乙", "甲 乙"},
		{"mixed_whitespace", " 甲\t乙\u3000丙\r\n> - 丁 \u0085戊", "甲 乙 丙 丁 戊"},
		{"tables_between_words", "|表格|\n首词\n|表格|\n末词", "首词 末词"},
		{"quoted_table", "> |甲|乙|\n尾词", "|甲|乙| 尾词"},
		{"empty_output", "|表格|\n|第二表|", ""},
		{"markers_only", ">>>\n>>", ""},
		{"single_field", "甲", "甲"},
		{"invalid_utf8", "甲\xff\u0085乙\xfe\n丙", "甲\xff 乙\xfe 丙"},
		{"zero_width_space", "甲\u200b乙\n丙", "甲\u200b乙 丙"},
		{"marker_then_unicode_space", "- \u00a0甲\t乙", "甲 乙"},
		{"trimmed_marker", "- \n正文", "- 正文"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqPlainText(tc.input); got != tc.want {
				t.Fatalf("正文分词拼接发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQPlainTextFieldOutputReleasesDiscardedInput(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("|t|\n", 32768) + "正文",
		strings.Repeat("\n", 32768) + "正文",
		strings.Repeat("> ", 32768) + "正文",
	} {
		got := qqPlainText(input)
		if got != "正文" {
			t.Fatalf("单字段结果不正确：%q", got)
		}
		// 比较原输入中字段的地址，确认短结果不再保留已经丢弃的大段内容。
		if unsafe.StringData(got) == unsafe.StringData(input[len(input)-len(got):]) {
			t.Fatal("短结果仍然引用已经丢弃的大段输入")
		}
	}
}

func TestQQPlainTextFieldOutputAllocationBudget(t *testing.T) {
	word := strings.Repeat("水", 32768)
	for _, tc := range []struct {
		name, input, want string
		budget            uint64
	}{
		{"many_words", strings.Repeat("词 ", 32768), strings.TrimSpace(strings.Repeat("词 ", 32768)), 256 << 10},
		{"many_lines", strings.Repeat("词\n", 32768), strings.TrimSpace(strings.Repeat("词 ", 32768)), 256 << 10},
		{"discarded_tables", strings.Repeat("|t|\n", 32768) + "正文", "正文", 64 << 10},
		{"two_large_fields", word + " " + word, word + " " + word, 256 << 10},
		{"single_large_field", word, word, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqPlainText(tc.input)
			runtime.ReadMemStats(&after)
			if got != tc.want {
				t.Fatal("长正文的分词拼接结果不正确")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("正文分词拼接分配 %d 字节", allocated)
			// 输入与预期在测量前构造；预算允许最终正文及运行时开销，不允许累计行或单词切片。
			if allocated > tc.budget {
				t.Fatalf("正文分词拼接分配 %d 字节，超过 %d 字节预算", allocated, tc.budget)
			}
		})
	}
}

func BenchmarkQQPlainTextFieldOutput(b *testing.B) {
	word := strings.Repeat("水", 32768)
	for _, tc := range []struct{ name, input string }{
		{"many_words", strings.Repeat("词 ", 32768)},
		{"many_lines", strings.Repeat("词\n", 32768)},
		{"discarded_tables", strings.Repeat("|t|\n", 32768) + "正文"},
		{"two_large_fields", word + " " + word},
		{"single_field", "普通正文"},
		{"ordinary", "第一词 第二词 第三词"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(tc.input)
			}
		})
	}
}
