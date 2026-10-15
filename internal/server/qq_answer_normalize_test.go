package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQNormalizeAnswerConditionalRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"empty", "", ""},
		{"plain", "打开设置后重试。", "打开设置后重试。"},
		{"newlines", "第一段\n\n第二段\n", "第一段\n\n第二段\n"},
		{"issue", "#123 已修复\n见 #456", "#123 已修复\n见 #456"},
		{"heading", "第一行\n## 标题\n正文", "第一行\n标题\n正文"},
		{"indented_heading", "第一行\n ## 标题", "第一行\n ## 标题"},
		{"heading_number", "# 123\n##456\n#789", "123\n##456\n#789"},
		{"heading_only", "#\n##\n末行", "\n\n末行"},
		{"heading_limit", "####### 标题", "# 标题"},
		{"heading_whitespace", "##\t\r\f标题", "标题"},
		{"unicode_space", "##\u3000标题", "\u3000标题"},
		{"curly_quotes", "“设置”里选‘双拼’", "「设置」里选『双拼』"},
		{"apostrophe", "don’t worry", "don't worry"},
		{"overlapping_apostrophes", "a’b’c", "a'b』c"},
		{"unmatched_apostrophe", "a’中 ’b 末’", "a』中 』b 末』"},
		{"literal_tab", `甲\t乙`, "甲 乙"},
		{"windows_path", `C:\temp\synthetic\test`, `C:\temp\synthetic\test`},
		{"windows_environment_path", `%APPDATA%\temp 甲\t乙`, `%APPDATA%\temp 甲\t乙`},
		{"markup", "**第一步**\t__重试__", "第一步 重试"},
		{"blank_lines", "\n\n\n甲\n\n\n\n乙\n\n\n", "\n\n甲\n\n乙\n\n"},
		{"heading_creates_blank_lines", "甲\n#\n##\n乙", "甲\n\n乙"},
		{"carriage_return", "甲\r\n\r\n\r\n乙", "甲\r\n\r\n\r\n乙"},
		{"invalid_utf8", "甲\xff\n乙\xfe", "甲\xff\n乙\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqNormalizeAnswer(tc.input); got != tc.want {
				t.Fatalf("答案格式规则发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQNormalizeAnswerNoopAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"single_line", strings.Repeat("水", 32768)},
		{"multiple_lines", strings.Repeat("普通回答\n\n", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqNormalizeAnswer(tc.input)
			runtime.ReadMemStats(&after)
			if got != tc.input {
				t.Fatal("无需规范化的答案发生变化")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("答案格式归一化分配 %d 字节", allocated)
			// 输入在测量前构造；预算允许运行时开销，排除无需转换时的正文复制与逐行切片。
			if allocated > 64<<10 {
				t.Fatalf("答案格式归一化分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func BenchmarkQQNormalizeAnswer(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short_plain", "打开设置后重试。"},
		{"answer_plain", strings.Repeat("可以在设置里切换输入方案。", 40)},
		{"answer_lines", strings.Repeat("可以在设置里切换输入方案。\n\n", 40)},
		{"long_plain", strings.Repeat("水", 32768)},
		{"long_lines", strings.Repeat("普通回答\n\n", 8192)},
		{"formatted", "## 设置方法\n**第一步**打开“设置”\n- 第二步 __重启__\n\n\n再试一次。"},
		{"apostrophes", "don’t worry, it’s fixed"},
		{"issue_lines", "#123 已修复\n#456 仍在处理"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqNormalizeAnswer(tc.input)
			}
		})
	}
}
