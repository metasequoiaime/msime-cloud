package server

import (
	"slices"
	"strings"
	"testing"
)

func previousQQProcessLabel(label string) bool {
	lower := strings.ToLower(label)
	return slices.Contains(qqProcessLabels, lower) || slices.ContainsFunc(qqProcessLabelPrefixes, func(p string) bool { return strings.HasPrefix(lower, p) })
}

func BenchmarkQQProcessLabel(b *testing.B) {
	for _, label := range []string{"bug", "PLATFORM/windows", "customer-request", "性能优化", "NEEDS-DESIGN"} {
		b.Run(label, func(b *testing.B) {
			b.Run("previous", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = previousQQProcessLabel(label)
				}
			})
			b.Run("current", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = qqProcessLabel(label)
				}
			})
		})
	}
}

func BenchmarkQQProcessLabelList(b *testing.B) {
	labels := []string{"bug", "PLATFORM/windows", "customer-request", "needs-design", "performance", "性能优化", "source/qq", "feature", "NEEDS-INFO", "question"}
	for _, impl := range []struct {
		name string
		run  func(string) bool
	}{{"previous", previousQQProcessLabel}, {"current", qqProcessLabel}} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, label := range labels {
					_ = impl.run(label)
				}
			}
		})
	}
}

func TestQQProcessLabelCompatibility(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  bool
	}{{"bug", true}, {"BUG", true}, {"Platform/windows", true}, {"needs-design", true}, {"customer-request", false}, {"İbug", false}, {"性能优化", false}} {
		if got := qqProcessLabel(tc.label); got != tc.want {
			t.Fatalf("%q=%v，期望 %v", tc.label, got, tc.want)
		}
	}
}
