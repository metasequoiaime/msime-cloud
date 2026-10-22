package server

import (
	"strings"
	"testing"
)

func previousQQLabelValidation(labels []string) bool {
	seen := map[string]bool{}
	for _, label := range labels {
		if label == "" || len(label) > 50 || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\r\n\t,") || seen[strings.ToLower(label)] {
			return false
		}
		seen[strings.ToLower(label)] = true
	}
	return true
}

func currentQQLabelValidation(labels []string) bool {
	seen := map[string]bool{}
	for _, label := range labels {
		lower := strings.ToLower(label)
		if label == "" || len(label) > 50 || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\r\n\t,") || seen[lower] {
			return false
		}
		seen[lower] = true
	}
	return true
}

func TestQQLabelValidationLowercaseCache(t *testing.T) {
	for _, labels := range [][]string{
		{"source/qq", "platform/windows", "bug"},
		{"Source/QQ", "Platform/Windows", "Bug"},
		{"问题", "建议", "文档"},
		{"bug", "BUG"},
	} {
		if got, want := currentQQLabelValidation(labels), previousQQLabelValidation(labels); got != want {
			t.Errorf("标签=%q：结果 %t，原结果 %t", labels, got, want)
		}
	}
}

var qqLabelValidationBenchmarkResult bool

func BenchmarkQQLabelValidationLowercaseCache(b *testing.B) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"小写", []string{"source/qq", "platform/windows", "bug"}},
		{"大写", []string{"Source/QQ", "Platform/Windows", "Bug"}},
		{"Unicode", []string{"问题", "建议", "文档"}},
	} {
		for _, impl := range []struct {
			name string
			fn   func([]string) bool
		}{
			{"原实现", previousQQLabelValidation},
			{"当前实现", currentQQLabelValidation},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqLabelValidationBenchmarkResult = impl.fn(tc.labels)
				}
			})
		}
	}
}
