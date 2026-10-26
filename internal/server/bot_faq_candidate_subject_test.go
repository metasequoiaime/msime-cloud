package server

import (
	"strings"
	"testing"
)

// 冻结优化前的候选去重主题构造，核对空白、非法 UTF-8 和长正文语义。
func previousQQFAQCandidateSubject(text string) string {
	return strings.Join(strings.Fields(text), "")
}

func TestQQFAQCandidateSubjectMatchesPrevious(t *testing.T) {
	for _, input := range []string{
		"", "普通问题", "  普通 问题  ", "\t候选\n框\r错位\f\v\u0085", "\u00a0水\u2003杉\u3000云", "😀\t输入法", "\xff普通\xfe 文本", strings.Repeat("长正文 ", 4096), strings.Repeat(" \t\n\r\f\u00a0\u2003", 128),
	} {
		got := qqFAQCandidateSubject(input)
		want := previousQQFAQCandidateSubject(input)
		if got != want {
			t.Errorf("候选去重主题变化：输入=%q，实际=%q，期望=%q", input, got, want)
		}
	}
}

func TestQQFAQCandidateSubjectAllocationBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("race 模式会显著增加基准分配")
	}
	for _, tc := range []struct {
		name, input string
		maxBytes    int64
	}{
		{"无空白", "普通问题候选框", 0},
		{"有空白", "普通问题 候选框\t错位", 64},
		{"长正文", strings.Repeat("普通问题 ", 4096), 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = qqFAQCandidateSubject(tc.input)
				}
			})
			if result.AllocedBytesPerOp() > tc.maxBytes {
				t.Fatalf("主题构造分配 %s，超过 %d 字节预算", result.MemString(), tc.maxBytes)
			}
		})
	}
}

func BenchmarkQQFAQCandidateSubject(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"无空白", "普通问题候选框"},
		{"有空白", "普通问题 候选框\t错位"},
		{"长正文", strings.Repeat("普通问题 ", 4096)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) string
		}{
			{"previous", previousQQFAQCandidateSubject},
			{"current", qqFAQCandidateSubject},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input)
				}
			})
		}
	}
}
