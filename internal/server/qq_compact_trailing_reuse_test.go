package server

import (
	"testing"
)

func TestQQCompactTrailingPrefixReusesInput(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"问号", "帮助？", "帮助"},
		{"表情", "最新版本🙏", "最新版本"},
		{"重复标点", "帮助？！。", "帮助"},
		{"标点后空白", "帮助？  \t", "帮助"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = qqCompact(tc.input) })
			if got != tc.want {
				t.Fatalf("命令规范化结果=%q，期望=%q", got, tc.want)
			}
			if allocations != 0 {
				t.Fatalf("已规范化前缀仍分配 %.1f 次，期望零分配", allocations)
			}
		})
	}
}
