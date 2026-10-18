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

func TestQQFeedbackMarkupEndBoundaries(t *testing.T) {
	for _, prefix := range []struct {
		text  string
		limit int
	}{{"<qqbot-at-user", 180}, {"<qqbot-at-everyone", 180}, {"<faceType=", 191}} {
		for _, unit := range []string{"a", "甲", "😀", string([]byte{0xff})} {
			for _, count := range []int{0, prefix.limit, prefix.limit + 1} {
				input := "前" + prefix.text + strings.Repeat(unit, count) + ">后"
				want := len(input) - len("后")
				if count > prefix.limit {
					want = -1
				}
				if got := qqFeedbackMarkupEnd(input, len("前")); got != want {
					t.Fatalf("标签=%q，字符=%q，数量=%d，得到%d，期望%d", prefix.text, unit, count, got, want)
				}
			}
		}
		for _, body := range []string{"空 格", "\t\r\f", ""} {
			input := prefix.text + body + ">"
			if got := qqFeedbackMarkupEnd(input, 0); got != len(input) {
				t.Fatalf("允许的空白被拒绝：%q，终点%d", input, got)
			}
		}
		for _, tail := range []string{"内容<>", "内容\n>", "内容"} {
			input := prefix.text + tail
			if got := qqFeedbackMarkupEnd(input, 0); got != -1 {
				t.Fatalf("无效标签被接受：%q，终点%d", input, got)
			}
		}
	}
	for _, input := range []string{"<@>", "<#a b>", "<emoji:a\t>"} {
		if got := qqFeedbackMarkupEnd(input, 0); got != -1 {
			t.Fatalf("其他标签校验发生变化：%q，终点%d", input, got)
		}
	}
}
