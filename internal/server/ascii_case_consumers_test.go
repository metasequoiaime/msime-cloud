package server

import (
	"strings"
	"testing"
)

func TestSharedASCIICaseLabelConsumers(t *testing.T) {
	for _, label := range []string{
		"", "unmatched", "bug", "BUG", "Type: Bug", "Kind/bug", "KIND/BUG", "TYPE: DOKS",
		"问题", "建议", "文档", "PLATFORM/中文", "Invalid", "DUPLICATE", "WONTFIX", "İbug", "A\xff", "\xffZ",
	} {
		labels := []string{label, "documentation", "duplicate"}
		if got, want := issueKind(labels), previousIssueKind(labels); got != want {
			t.Errorf("类型标签 %q = %q，期望 %q", labels, got, want)
		}
		if got, want := qqIssueState("closed", "completed", labels), previousQQIssueState("closed", "completed", labels); got != want {
			t.Errorf("状态标签 %q = %q，期望 %q", labels, got, want)
		}
		if got, want := qqProcessLabel(label), previousQQProcessLabel(label); got != want {
			t.Errorf("流程标签 %q = %t，期望 %t", label, got, want)
		}
	}
}

func TestSharedASCIICasePlatformWordConsumer(t *testing.T) {
	for _, word := range []string{"", "windows", "Windows", "IOS", "İOS", "KIND", "A中文", "中文Z", "A\xff", "\xffZ"} {
		for _, question := range []string{"", "unmatched", "before " + strings.ToLower(word) + " after"} {
			want := word != "" && strings.Contains(question, strings.ToLower(word))
			if got := qqContainsLowerPlatformWord(question, word); got != want {
				t.Errorf("平台词 %q 在 %q 中 = %t，期望 %t", word, question, got, want)
			}
		}
	}
}
