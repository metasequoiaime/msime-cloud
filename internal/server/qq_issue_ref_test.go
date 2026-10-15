package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQIssueRefConditionalRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "候选框在哪里", ""},
		{"issue", "#818", "818"},
		{"repo_issue", "msime-windows #818", "818"},
		{"fullwidth_issue", "＃８１８", "818"},
		{"trimmed", "  #818  ", "818"},
		{"sentence", "#818 修好了吗", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := qqIssueRef(tc.input)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("普通消息误识别为 issue：%v", got)
				}
				return
			}
			if got == nil || got[2] != tc.want {
				t.Fatalf("issue 命令解析结果发生变化：结果=%v，预期编号=%q", got, tc.want)
			}
		})
	}
}

func TestQQIssueRefNoopAllocationBudget(t *testing.T) {
	input := strings.Repeat("候选框在哪里", 1024)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if got := qqIssueRef(input); got != nil {
		t.Fatal("普通消息误识别为 issue")
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<10 {
		t.Fatalf("普通消息 issue 检查分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}

func BenchmarkQQIssueRef(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short_plain", "候选框在哪里"},
		{"plain", strings.Repeat("候选框在哪里", 1024)},
		{"issue", "#818"},
		{"fullwidth_issue", "＃８１８"},
		{"sentence", "#818 修好了吗"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqIssueRef(tc.input)
			}
		})
	}
}
