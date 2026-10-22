package server

import (
	"strings"
	"testing"
)

func previousIssueKind(labels []string) string {
	for _, l := range labels {
		switch strings.ToLower(l) {
		case "bug", "问题", "type: bug", "kind/bug":
			return "bug"
		case "enhancement", "feature", "feature request", "idea", "suggestion", "建议", "type: feature", "kind/feature":
			return "idea"
		case "documentation", "docs", "文档", "type: docs", "kind/docs":
			return "docs"
		}
	}
	return ""
}
func TestIssueKindFastPathCompatibility(t *testing.T) {
	for _, labels := range [][]string{{"Bug"}, {"FEATURE"}, {"Kind/Docs"}, {"问题"}, {"建议"}, {"错误", "Documentation"}, {"none"}, {"\xff", "bug"}} {
		if got, want := issueKind(labels), previousIssueKind(labels); got != want {
			t.Fatalf("%q: %q != %q", labels, got, want)
		}
	}
}
func BenchmarkIssueKindFastPath(b *testing.B) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{{"lower", []string{"bug"}}, {"upper", []string{"Feature Request"}}, {"none", []string{"other", "label"}}, {"unicode", []string{"问题"}}} {
		for _, impl := range []struct {
			name string
			fn   func([]string) string
		}{{"previous", previousIssueKind}, {"current", issueKind}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.fn(tc.labels)
				}
			})
		}
	}
}
