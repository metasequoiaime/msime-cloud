package server

import (
	"strings"
	"testing"
)

func TestQQPlainTextReuseCandidate(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"普通正文", true},
		{"第一词 第二词", true},
		{"", true},
		{" 需要裁剪", false},
		{"有  两个空格", false},
		{"甲\n乙", false},
		{"**加粗**", false},
		{"[链接](https://example.test)", false},
		{"甲\u3000乙", false},
	} {
		if got := qqPlainTextCanReuse(tc.input); got != tc.want {
			t.Errorf("qqPlainTextCanReuse(%q) = %v，预期 %v", tc.input, got, tc.want)
		}
	}
}

func TestQQPlainTextNormalizedKeepsRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "甲 乙", "甲 乙"},
		{"tab", "甲\t乙", "甲 乙"},
		{"newline", "甲\n乙", "甲 乙"},
		{"carriage_return", "甲\r乙", "甲 乙"},
		{"form_feed", "甲\f乙", "甲 乙"},
		{"vertical_tab", "甲\v乙", "甲 乙"},
		{"vertical_tab_marker", "#\v正文 尾词", "# 正文 尾词"},
		{"unicode_space", "甲\u0085乙\u3000丙", "甲 乙 丙"},
		{"header", "# 正文 尾词", "正文 尾词"},
		{"literal_markers", "#标签 星*号 下_划线", "#标签 星*号 下_划线"},
		{"formatting", "**甲** 乙", "甲 乙"},
		{"link", "[甲](https://example.com) 乙", "甲 乙"},
		{"table", "|甲|乙|", ""},
		{"quoted_table", "> |甲| 乙", "|甲| 乙"},
		{"invalid_utf8", "甲\xff 乙\xfe", "甲\xff 乙\xfe"},
		{"nonspace_control", "甲\x00 乙\x7f", "甲\x00 乙\x7f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqPlainText(tc.input); got != tc.want {
				t.Fatalf("规范化正文结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQPlainTextNormalizedAllocationBudget(t *testing.T) {
	word := strings.Repeat("水", 32768)
	for _, tc := range []struct{ name, input string }{
		{"short", "第一词 第二词 第三词"},
		{"many_words", strings.TrimSpace(strings.Repeat("词 ", 32768))},
		{"large_fields", word + " " + word},
		{"invalid_utf8", "甲\xff 乙\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() {
				got = qqPlainText(tc.input)
			})
			if got != tc.input {
				t.Fatal("已规范化正文发生变化")
			}
			t.Logf("已规范化正文分配 %.1f 次", allocations)
			// 输入已符合输出格式，转换时不应再构建正文副本。
			if allocations != 0 {
				t.Fatalf("已规范化正文分配 %.1f 次，预期零分配", allocations)
			}
		})
	}
}

func BenchmarkQQPlainTextNormalized(b *testing.B) {
	word := strings.Repeat("水", 32768)
	for _, tc := range []struct{ name, input string }{
		{"short", "第一词 第二词 第三词"},
		{"many_words", strings.TrimSpace(strings.Repeat("词 ", 32768))},
		{"large_fields", word + " " + word},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(tc.input)
			}
		})
	}
}
