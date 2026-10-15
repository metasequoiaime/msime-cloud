package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQTextConditionalTransforms(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "普通正文", "普通正文"},
		{"single_markers", "a*b c_d", "a*b c_d"},
		{"markdown_url_text", "[https://example.com](https://example.test) 末", "末"},
		{"multiline_link", "[跨\n行](https://example.com)", "跨 行"},
		{"unclosed_link", "[文字](未闭合", "[文字](未闭合"},
		{"separated_link", "[文字] (https://example.com)", "[文字] ("},
		{"invalid_utf8", "前\xff后\xfe", "前\xff后\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqPlainText(tc.input); got != tc.want {
				t.Fatalf("条件转换影响了正文：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct{ input, want string }{
		{"\u3000普通正文\u0085", "普通正文"},
		{"甲\t乙", "甲\t乙"},
		{"甲 \t 乙", "甲 乙"},
		{"甲 http://example.com 乙", "甲 乙"},
		{"甲\n乙", "甲\n乙"},
		{"9://abc", "9://abc"},
		{"前git+ssh://example.com/path后", "前后"},
		{"甲 A:// 乙", "甲 乙"},
		{"甲https://example.com\u00a0乙", "甲\u00a0乙"},
	} {
		if got := qqStripLinks(tc.input); got != tc.want {
			t.Fatalf("条件过滤链接发生变化：输入=%q，结果=%q，预期=%q", tc.input, got, tc.want)
		}
	}
}

func TestQQTextNoopAllocationBudget(t *testing.T) {
	text := strings.Repeat("水", 32768)
	for _, tc := range []struct {
		name string
		call func(string) string
	}{
		{"plain_text", qqPlainText},
		{"strip_links", qqStripLinks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := tc.call(text)
			runtime.ReadMemStats(&after)
			if got != text {
				t.Fatal("纯文本内容发生变化")
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("纯文本转换分配 %d 字节", allocated)
			// 输入在测量前构造；预算允许运行时开销，不允许为无匹配的固定规则复制整段正文。
			if allocated > 64<<10 {
				t.Fatalf("纯文本转换分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func BenchmarkQQTextConditionalTransforms(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"long_plain", strings.Repeat("水", 32768)},
		{"short_plain", "普通正文"},
		{"formatted", "**加粗** __强调__ `代码`"},
		{"links", "[链接](https://example.com) 详见https://example.test。"},
		{"spaced_words", strings.Repeat("词 ", 32768)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(tc.input)
			}
		})
	}
}
