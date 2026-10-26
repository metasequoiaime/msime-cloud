package server

import (
	"math/rand"
	"strings"
	"testing"
)

func TestQQContainsQuestionMatchesPrevious(t *testing.T) {
	check := func(content string) {
		t.Helper()
		if got, want := qqContainsQuestion(content), previousQQCommandQuestion.MatchString(content); got != want {
			t.Fatalf("全文问句判断变化：内容=%q，结果=%t，期望=%t", content, got, want)
		}
	}
	for _, term := range []string{"怎么", "怎样", "如何", "为什么", "为啥", "吗", "呢", "吧", "在哪", "哪里", "哪儿", "什么", "是不是", "能不能", "可不可以", "不了", "不生效", "没用", "失败", "?", "？"} {
		for end := range len(term) + 1 {
			for _, head := range []string{"", "普通正文", "\n", "\u3000", "\ufffd", "\xff", "\xe4\xb8"} {
				check(head + term[:end] + "普通正文")
			}
		}
		for value := range 256 {
			check(string([]byte{byte(value)}) + term)
			check(term + string([]byte{byte(value)}))
		}
	}
	for _, tc := range qqQuestionTermsCases() {
		check(tc.text)
	}
	random := rand.New(rand.NewSource(976))
	units := []string{"怎", "怎么", "如", "何", "为什么", "呢", "吧", "在哪", "可不", "可以", "不了", "不生效", "失败", "?", "？", "普通正文", "A", "\n", "\u3000", "\ufffd", "\xff", "\xe4\xb8", "😀"}
	for range 4000 {
		var content strings.Builder
		for range random.Intn(60) {
			content.WriteString(units[random.Intn(len(units))])
		}
		check(content.String())
		raw := make([]byte, random.Intn(150))
		_, _ = random.Read(raw)
		check(string(raw))
	}
}

func TestQQContainsQuestionAvoidsAllocation(t *testing.T) {
	for _, tc := range qqQuestionTermsCases() {
		var got bool
		allocations := testing.AllocsPerRun(10, func() { got = qqContainsQuestion(tc.text) })
		if want := previousQQCommandQuestion.MatchString(tc.text); got != want {
			t.Fatalf("场景=%s，结果=%t，期望=%t", tc.name, got, want)
		}
		if allocations != 0 {
			t.Errorf("场景=%s，分配 %g 次，期望零分配", tc.name, allocations)
		}
	}
}

func qqQuestionTermsCases() []struct{ name, text string } {
	body := strings.Repeat("详细说明", 500)
	return []struct{ name, text string }{
		{"空正文", ""},
		{"短普通正文", "候选框位置错误"},
		{"短问句", "怎么用"},
		{"首部问句长正文", "怎么用" + body},
		{"首部失败长正文", "失败" + body},
		{"靠前问句长正文", "功能失败" + body},
		{"长普通正文", body},
		{"尾部问句长正文", body + "怎么用？"},
		{"ASCII长正文", strings.Repeat("simple text ", 500)},
		{"无效UTF8", "\xff" + body},
		{"重复部分前缀", strings.Repeat("可不", 1000)},
	}
}

func BenchmarkQQQuestionTerms(b *testing.B) {
	for _, tc := range qqQuestionTermsCases() {
		for _, impl := range []struct {
			name  string
			match func(string) bool
		}{
			{"Previous", previousQQCommandQuestion.MatchString}, {"Current", qqContainsQuestion},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.match(tc.text)
				}
			})
		}
	}
}
