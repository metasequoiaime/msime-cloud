package server

import (
	"strings"
	"testing"
)

func previousQQIssueState(state, reason string, labels []string) string {
	if state != "open" {
		for _, label := range labels {
			switch strings.ToLower(label) {
			case "invalid":
				return "已关闭（无效）"
			case "duplicate":
				return "已关闭（重复）"
			case "wontfix":
				return "已关闭（不修）"
			}
		}
	}
	switch {
	case state == "open":
		return "还没解决"
	case reason == "completed":
		return "已完成"
	case reason == "not_planned":
		return "不计划处理"
	default:
		return "已关闭"
	}
}

func TestQQIssueStateLabelFastPathMatchesPrevious(t *testing.T) {
	for _, tc := range []struct {
		state, reason string
		labels        []string
	}{
		{"closed", "completed", []string{"bug", "Invalid"}},
		{"closed", "not_planned", []string{"duplicate"}},
		{"closed", "", []string{"WONTFIX"}},
		{"closed", "", []string{"错误", "Duplicate"}},
		{"closed", "", []string{"\xff", "wontfix"}},
		{"closed", "", []string{"bug", "feature"}},
		{"open", "", []string{"Invalid"}},
	} {
		if got, want := qqIssueState(tc.state, tc.reason, tc.labels), previousQQIssueState(tc.state, tc.reason, tc.labels); got != want {
			t.Errorf("状态=%q 原因=%q 标签=%q：结果 %q，原结果 %q", tc.state, tc.reason, tc.labels, got, want)
		}
	}
}

var qqIssueStateBenchmarkResult string

func BenchmarkQQIssueStateLabelFold(b *testing.B) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"小写命中", []string{"bug", "duplicate"}},
		{"大写命中", []string{"bug", "Duplicate"}},
		{"无匹配", []string{"bug", "feature", "help"}},
		{"Unicode", []string{"错误", "duplicate"}},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string, string, []string) string
		}{
			{"原实现", previousQQIssueState},
			{"当前实现", qqIssueState},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqIssueStateBenchmarkResult = impl.fn("closed", "", tc.labels)
				}
			})
		}
	}
}
