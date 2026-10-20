package server

import (
	"math/rand"
	"runtime"
	"slices"
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

func previousQQIssueRef(text string) []string {
	if strings.IndexByte(text, '#') < 0 && !strings.Contains(text, "＃") {
		return nil
	}
	return qqIssueRefPattern.FindStringSubmatch(strings.TrimSpace(qqFoldWidth(text)))
}

func TestQQIssueRefMatchesPreviousFastPath(t *testing.T) {
	inputs := []string{
		"#818", "msime-windows #818", "  #818  ", "#818 修好了吗", "repo/name#1", "repo/name #0000001",
		"#12345678", "repo name #1", "＃８１８", "中文 #818", "#\xff", "#1\u3000",
	}
	random := rand.New(rand.NewSource(20261020))
	units := []string{"", "repo", "/", "-", "_", ".", "#", "＃", "1", "8", " ", "\t", "中文", "\xff", "？"}
	for range 5000 {
		var text strings.Builder
		for range random.Intn(40) {
			text.WriteString(units[random.Intn(len(units))])
		}
		inputs = append(inputs, text.String())
	}
	for _, input := range inputs {
		got, want := qqIssueRef(input), previousQQIssueRef(input)
		if !slices.Equal(got, want) {
			t.Fatalf("issue 引用结果变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQIssueRefASCIIFastPathAllocationBudget(t *testing.T) {
	const input = "msime-windows #818"
	if allocations := testing.AllocsPerRun(100, func() {
		if got := qqIssueRef(input); got == nil || got[2] != "818" {
			t.Fatalf("issue 引用未解析：%q", got)
		}
	}); allocations > 1 {
		t.Fatalf("ASCII issue 引用分配 %.0f 次，超过 1 次预算", allocations)
	}
}

func BenchmarkQQIssueRefComparison(b *testing.B) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"ascii", "msime-windows #818"},
		{"plain", strings.Repeat("候选框在哪里", 1024)},
		{"fullwidth", "＃８１８"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string) []string
		}{
			{"previous", previousQQIssueRef}, {"current", qqIssueRef},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.fn(tc.input)
				}
			})
		}
	}
}
