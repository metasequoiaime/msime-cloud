package server

import (
	"strings"
	"testing"
)

func TestQQNormalizeWhitespaceSingleFieldAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"边界内", strings.Repeat("a", 128<<10)},
		{"边界外", strings.Repeat("a", (128<<10)+1)},
		{"长英文", strings.Repeat("a", 1<<20)},
		{"长中文", strings.Repeat("普通正文", 16384)},
		{"无效字节", strings.Repeat("正文\xff", 32768)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "\t\u3000" + tc.text + "\u0085\n"
			var got string
			allocs := testing.AllocsPerRun(10, func() {
				got = qqNormalizeWhitespace(input)
			})
			if got != tc.text {
				t.Fatalf("单字段规范化改变了正文：结果长度=%d，期望长度=%d", len(got), len(tc.text))
			}
			if allocs != 0 {
				t.Fatalf("没有内部空白时应复用裁剪后的正文，实际分配 %.0f 次", allocs)
			}
		})
	}
}
