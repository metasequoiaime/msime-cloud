package server

import (
	"runtime"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
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

func previousQQPlainTextCanReuse(md string) bool {
	if md == "" {
		return true
	}
	if strings.Contains(md, "](") || strings.Contains(md, "://") || strings.Contains(md, "**") || strings.Contains(md, "__") || strings.ContainsRune(md, '`') {
		return false
	}
	if md[0] == '|' || qqLineMarkerEnd(md) != 0 {
		return false
	}
	previousSpace := false
	fast := true
	for i := 0; i < len(md); i++ {
		b := md[i]
		if b >= utf8.RuneSelf {
			switch b {
			case 0xc2, 0xe1, 0xe2, 0xe3:
				fast = false
			}
			previousSpace = false
			continue
		}
		if b == ' ' {
			if previousSpace || i == 0 || i+1 == len(md) {
				return false
			}
			previousSpace = true
			continue
		}
		if b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r' {
			return false
		}
		previousSpace = false
	}
	if fast {
		return true
	}
	previousSpace = false
	for offset := 0; offset < len(md); {
		r, size := utf8.DecodeRuneInString(md[offset:])
		if unicode.IsSpace(r) {
			if r != ' ' || previousSpace || offset == 0 || offset+size == len(md) {
				return false
			}
			previousSpace = true
		} else {
			previousSpace = false
		}
		offset += size
	}
	return true
}

func TestQQPlainTextCanReuseAvoidsRepeatedScans(t *testing.T) {
	for _, text := range []string{
		"", "普通正文", "普通 正文", "普通\n正文", "普通\t正文", "**加粗**", "[链接](https://example.test)",
		"reader@example.test", "甲\u3000乙", "甲\u0085乙", "甲\xff乙", "- 列表", "|表格|", "普通://网址",
	} {
		if got, want := qqPlainTextCanReuse(text), previousQQPlainTextCanReuse(text); got != want {
			t.Fatalf("可复用判定变化：输入=%q，结果=%t，期望=%t", text, got, want)
		}
	}
	text := strings.Repeat("词\n", 32768)
	baseline := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_ = previousQQPlainTextCanReuse(text)
		}
	})
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_ = qqPlainTextCanReuse(text)
		}
	})
	if result.NsPerOp() >= baseline.NsPerOp()*8/10 {
		t.Fatalf("长多行正文仍重复扫描：原=%s，新=%s", baseline, result)
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
