package server

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"unsafe"
)

func previousQQRedactContacts(text string) string {
	for _, pattern := range qqContactPatterns {
		text = pattern.ReplaceAllString(text, qqContactRedacted)
	}
	return text
}

func previousQQGapTopic(topic string) string {
	topic = previousQQRedactContacts(strings.Join(strings.Fields(topic), " "))
	if runes := []rune(topic); len(runes) > qqGapRunes {
		topic = string(runes[:qqGapRunes])
	}
	return topic
}

func TestQQRedactContactsUnchangedAllocationBudget(t *testing.T) {
	text := strings.Repeat("普通正文", 20000)
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqRedactContacts(text)
		}
	})
	if got != text || result.AllocedBytesPerOp() > 64<<10 {
		t.Fatalf("未匹配的脱敏不应复制完整正文：%s", result.MemString())
	}
}

func qqGapTopicInputs() []string {
	return []string{
		"", " \t\u3000", "普通问题", " 候选框\u0085正文\u3000问题 ",
		"联系 reader@example.test 和 QQ 00000123，微信号：example_user",
		strings.Repeat("a", qqGapRunes+20) + "@example.test",
		strings.Repeat("字", qqGapRunes-2) + " reader@example.test 正文",
		strings.Repeat("字", qqGapRunes), strings.Repeat("字", qqGapRunes+1),
		"\xff" + strings.Repeat("字", qqGapRunes+1),
		strings.Repeat("字", qqGapRunes) + "\xff", "\xff 短正文",
	}
}

func TestQQGapTopicMatchesPrevious(t *testing.T) {
	texts := qqGapTopicInputs()
	random := rand.New(rand.NewSource(660))
	units := []string{"正文", "候选", "a", "QQ", "微信号：", "reader@example.test", "00000123", "wx:example_user", " ", "\t", "\n", "\v", "\f", "\u0085", "\u00a0", "\u3000", "\ufffd", "\xff", "\xe6", "\x8f", "\x90", "\x00"}
	for range 500 {
		var text strings.Builder
		for range random.Intn(120) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		if got, want := qqRedactContacts(text), previousQQRedactContacts(text); got != want {
			t.Fatalf("脱敏结果变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
		if got, want := qqGapTopic(text), previousQQGapTopic(text); got != want {
			t.Fatalf("概括预处理变化：输入=%q，结果=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQGapTopicBoundedContactSemantics(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		check func(t *testing.T, got string)
	}{
		{
			name: "联系方式在摘要前",
			text: "reader@example.test " + strings.Repeat("字", qqGapRunes),
			check: func(t *testing.T, got string) {
				if strings.Contains(got, "reader@example.test") {
					t.Fatal("摘要前的联系方式未脱敏")
				}
				if got != previousQQGapTopic("reader@example.test "+strings.Repeat("字", qqGapRunes)) {
					t.Fatal("摘要前的联系方式改变了摘要结果")
				}
			},
		},
		{
			name: "联系方式在摘要后",
			text: strings.Repeat("字", qqGapRunes+20) + " reader@example.test",
			check: func(t *testing.T, got string) {
				want := previousQQGapTopic(strings.Repeat("字", qqGapRunes+20) + " reader@example.test")
				if got != want {
					t.Fatal("摘要后的联系方式改变了摘要结果")
				}
			},
		},
		{
			name: "联系方式跨摘要边界",
			text: strings.Repeat("字", qqGapRunes-5) + "reader@example.test",
			check: func(t *testing.T, got string) {
				want := previousQQGapTopic(strings.Repeat("字", qqGapRunes-5) + "reader@example.test")
				if got != want {
					t.Fatalf("跨边界联系方式改变了摘要结果：%q，期望 %q", got, want)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, qqGapTopic(tc.text))
		})
	}
}

func TestQQNormalizeWhitespacePrefixStopsAtLimit(t *testing.T) {
	input := strings.Repeat("字", qqGapRunes) + strings.Repeat(" ", 1<<20)
	got, truncated := qqNormalizeWhitespacePrefix(input, qqGapRunes)
	if got != strings.Repeat("字", qqGapRunes) || !truncated {
		t.Fatalf("达到摘要上限后的空白应视为已截断：结果长度=%d，截断=%v", len([]rune(got)), truncated)
	}
}

func TestQQGapTopicContactScanStopsAtLimit(t *testing.T) {
	leading := strings.Repeat("字", qqGapRunes)
	if qqGapTopicMayContainContact(leading + " reader@example.test") {
		t.Fatal("摘要上限之后的联系方式不应触发整段脱敏")
	}
	if !qqGapTopicMayContainContact(leading[:len(leading)-len("字")] + " reader@example.test") {
		t.Fatal("摘要上限之前的联系方式必须触发脱敏")
	}
}

func TestQQGapTopicRateLimitSubject(t *testing.T) {
	s, fake, limiter := qqServer(t)
	s.qq.config.GapsRepo = "o/docs"
	for _, text := range qqGapTopicInputs()[2:] {
		want := previousQQGapTopic(text)
		limiter.counts = map[string]int{"qq-bot-gap|" + want: 1}
		if !s.recordQQGap(context.Background(), "群友问：", text) || limiter.counts["qq-bot-gap|"+want] != 1 || len(fake.comments) != 0 {
			t.Fatalf("去重键变化或重复发布：输入=%q，限流=%v", text, limiter.counts)
		}
	}
}

func qqGapTopicAllocationCases() []struct {
	name, text string
	budget     int64
} {
	return []struct {
		name, text string
		budget     int64
	}{
		{"长正文", strings.Repeat("普通正文", 20000), 64 << 10},
		{"末尾联系方式", strings.Repeat("普通正文", 20000) + " reader@example.test", 512 << 10},
		{"密集空白", strings.Repeat("普通 \t正文\u3000", 10000), (140 << 10) + (64 << 10)},
		{"大量首尾空白", strings.Repeat(" ", 1<<20) + "普通问题" + strings.Repeat("\u3000", 1<<18), 64 << 10},
		{"达到上限后长空白", strings.Repeat("字", qqGapRunes) + strings.Repeat(" ", 1<<20), 64 << 10},
		{"全部空白", strings.Repeat(" \t\u3000", 10000), 0},
	}
}

func TestQQGapTopicAllocationBudget(t *testing.T) {
	for _, tc := range qqGapTopicAllocationCases() {
		t.Run(tc.name, func(t *testing.T) {
			want := previousQQGapTopic(tc.text)
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqGapTopic(tc.text)
				}
			})
			if got != want || result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("概括结果变化或超出分配预算：分配=%s，预算=%d", result.MemString(), tc.budget)
			}
		})
	}
}

func TestQQGapTopicAllWhitespace(t *testing.T) {
	for _, unit := range []string{" ", "\t\r\n\f\v", "\u3000", " \t\u3000", "\u0085\u00a0\u1680\u202f"} {
		if got := qqGapTopic(strings.Repeat(unit, 10000)); got != "" {
			t.Fatalf("全空白概括应为空：空白单元=%q，结果=%q", unit, got)
		}
	}
}

func BenchmarkQQGapTopic(b *testing.B) {
	cases := qqGapTopicAllocationCases()
	cases = append(cases, struct {
		name, text string
		budget     int64
	}{"末尾联系方式", strings.Repeat("普通正文", 20000) + " reader@example.test", 0})
	for _, implementation := range []struct {
		name string
		run  func(string) string
	}{
		{"previous", previousQQGapTopic}, {"current", qqGapTopic},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			for _, tc := range cases {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = implementation.run(tc.text)
					}
				})
			}
		})
	}
}

func TestQQGapTopicDoesNotRetainLongInput(t *testing.T) {
	padding := strings.Repeat(" ", 1<<20)
	for _, body := range []string{"普通问题", strings.Repeat("字", qqGapRunes+1)} {
		input := padding + body + padding
		got := qqGapTopic(input)
		if got != previousQQGapTopic(input) {
			t.Fatal("概括结果变化")
		}
		if unsafe.StringData(got) == unsafe.StringData(input[len(padding):]) {
			t.Fatal("短概括仍引用长输入的底层内存")
		}
	}
}
