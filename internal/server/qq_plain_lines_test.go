package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQPlainTextLineBuffersKeepContent(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"empty_lines", "\n\n首行\n\n第二行\n", "首行 第二行"},
		{"multiline_link", "[第一\n第二](https://example.com)\n尾行", "第一 第二 尾行"},
		{"link_across_fence", "[第一\n```text\n被移除\n```\n第二](https://example.com)", "第一 第二"},
		{"empty_link_lines", "[\n\n- 链接文字\n](https://example.com)", "链接文字"},
		{"fence_and_empty_lines", "\n```\n不保留\n```\n\n可见\n", "可见"},
		{"inline_backticks", "正文 ```inline``` 尾行", "正文 inline 尾行"},
		{"late_fence", "\n首行\r\n```go\n忽略\n```\r\n尾行\n", "首行 尾行"},
		{"multiple_fences", "首行\n```\n忽略一\n```\n中间\n```\n忽略二\n```\n尾行", "首行 中间 尾行"},
		{"unclosed_fence", "首行\n```\n忽略", "首行"},
		{"table", "\n| a | b |\n\n> | 正文 |\n尾行", "| 正文 | 尾行"},
		{"invalid_utf8", "前\xff\u0085后\xfe\t末", "前\xff 后\xfe 末"},
		{"crlf", "首行\r\n- 二行\r\n", "首行 二行"},
		{"重复行", "正文\n正文\n", "正文 正文"},
		{"crlf", "首行\r\n- 二行\r\n", "首行 二行"},
		{"whitespace", "\u3000\n\t\n\u0085", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqPlainText(tc.input); got != tc.want {
				t.Fatalf("逐行正文转换发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQPlainTextLineBufferAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		budget      uint64
	}{
		{"empty_lines", strings.Repeat("\n", 65536), 2 << 20},
		{"filtered_empty_lines", strings.Repeat("\n", 32768) + "```\nignored\n```\n" + strings.Repeat("\n", 32768), 2 << 20},
		{"code_only", "```text\n" + strings.Repeat("ignored\n", 65536) + "```", 256 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqPlainText(tc.input)
			runtime.ReadMemStats(&after)
			if got != "" {
				t.Fatal("被过滤的正文不应产生输出")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("多行正文转换分配 %d 字节", allocated)
			// 输入在测量前构造；预算排除按全部输入行数保留切片及为被过滤正文预留大缓冲。
			if allocated > tc.budget {
				t.Fatalf("多行正文转换分配 %d 字节，超过 %d 字节预算", allocated, tc.budget)
			}
		})
	}
}

func BenchmarkQQPlainTextLineBuffers(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"empty_lines", strings.Repeat("\n", 65536)},
		{"short_lines", strings.Repeat("正文\n", 16384)},
		{"many_words", strings.Repeat("词 ", 32768)},
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
