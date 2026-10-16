package server

import (
	"math/rand"
	"strings"
	"testing"
)

func TestQQFeedbackTextMatchesPattern(t *testing.T) {
	random := rand.New(rand.NewSource(12))
	units := []string{"", "<", ">", "@", "#", "emoji:", "qqbot-at-user", "faceType=", " ", "\n", "a", "1", ",", "甲"}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		want := strings.TrimSpace(qqMarkupPattern.ReplaceAllString(input, ""))
		if got := qqFeedbackText(input); got != want {
			t.Fatalf("QQ 标记清理不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

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
