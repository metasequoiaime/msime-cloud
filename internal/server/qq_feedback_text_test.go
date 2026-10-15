package server

import (
	"strings"
	"testing"
)

func TestQQFeedbackMarkupCandidate(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"普通消息", false},
		{"<qqbot-at-user>问题", true},
		{"比较 a < b", true},
	} {
		if got := qqFeedbackMarkupCandidate(tc.input); got != tc.want {
			t.Errorf("qqFeedbackMarkupCandidate(%q) = %v，预期 %v", tc.input, got, tc.want)
		}
	}
}

func BenchmarkQQFeedbackText(b *testing.B) {
	for _, tc := range []struct {
		name, input string
	}{
		{"plain", strings.Repeat("普通消息内容。", 4096)},
		{"markup", strings.Repeat("普通消息内容。", 4096) + "<qqbot-at-user>"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqFeedbackText(tc.input)
			}
		})
	}
}
