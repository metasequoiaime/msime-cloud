package server

import (
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestQQLineMarkerEndMatchesPreviousPattern(t *testing.T) {
	previous := regexp.MustCompile(`^(?:#{1,6}\s|>\s?|[-*+]\s|\d+\.\s)`)
	for _, prefix := range []string{"", ">", "-", "*", "+", "#", "######", "#######", "1", "01", "123.", "９", "1.", "\xff", strings.Repeat("9", 512) + "."} {
		for separator := range 256 {
			for _, tail := range []string{"", "正文", "\xff", "> - 正文"} {
				line := prefix + string([]byte{byte(separator)}) + tail
				want := len(previous.FindString(line))
				if got := qqLineMarkerEnd(line); got != want {
					t.Fatalf("行首标记不兼容：输入=%q，结果=%d，预期=%d", line, got, want)
				}
			}
		}
	}
	for _, line := range []string{"", ">", "#", "1.", "-", ">\u3000正文", "#\u0085正文", "-\u00a0正文"} {
		if got, want := qqLineMarkerEnd(line), len(previous.FindString(line)); got != want {
			t.Fatalf("行首边界不兼容：输入=%q，结果=%d，预期=%d", line, got, want)
		}
	}
}

func TestQQPlainTextNestedMarkerRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"nested", ">> - 12. #### 正文", "正文"},
		{"long_number", strings.Repeat("9", 512) + ". 正文", "正文"},
		{"too_many_hashes", "####### 正文", "####### 正文"},
		{"missing_space", "12.正文", "12.正文"},
		{"unicode_digit", "１２. 正文", "１２. 正文"},
		{"unicode_space", "-\u00a0正文", "- 正文"},
		{"vertical_tab", "#\v正文", "# 正文"},
		{"form_feed", "#\f正文", "正文"},
		{"nested_unicode_space", ">\u3000- \u3000## 正文", "正文"},
		{"quoted_table", "> | a | b", "| a | b"},
		{"table", "| a | b", ""},
		{"invalid_utf8", "- \xff正文", "\xff正文"},
		{"plus", "+ 正文", "正文"},
		{"asterisk", "* 正文", "正文"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqPlainText(tc.input); got != tc.want {
				t.Fatalf("行首标记处理发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQPlainTextNestedMarkerAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"deep", strings.Repeat("> ", 2048) + "正文", "正文"},
		{"long_body", strings.Repeat("> ", 512) + strings.Repeat("正文", 8192), strings.Repeat("正文", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqPlainText(tc.input)
			runtime.ReadMemStats(&after)
			if got != tc.want {
				t.Fatal("嵌套标记影响了正文")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("嵌套标记解析分配 %d 字节", allocated)
			// 输入在测量前构造；预算允许正文及运行时开销，不允许逐层复制剩余整行。
			if allocated > 2<<20 {
				t.Fatalf("嵌套标记解析分配 %d 字节，超过 2 MiB 预算", allocated)
			}
		})
	}
}

func BenchmarkQQPlainTextNestedMarkers(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"deep", strings.Repeat("> ", 2048) + "正文"},
		{"long_body", strings.Repeat("> ", 512) + strings.Repeat("正文", 8192)},
		{"ordinary", "> - 第一条"},
		{"plain", "普通正文"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(tc.input)
			}
		})
	}
}
