package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

var previousQQCommandQuestion = regexp.MustCompile(`怎么|怎样|如何|为什么|为啥|吗|呢|吧|在哪|哪里|哪儿|什么|是不是|能不能|可不可以|不了|不生效|没用|失败|[?？]`)

func previousQQCommandAsks(content string, glued bool) bool {
	if loc := previousQQCommandQuestion.FindStringIndex(content); loc != nil && loc[0] == 0 {
		return true
	}
	if glued {
		for _, prefix := range []string{"后", "了", "的", "条", "功能", "库", "不"} {
			if strings.HasPrefix(content, prefix) {
				return true
			}
		}
	}
	return false
}

func TestQQCommandAsksPrefixCompatibility(t *testing.T) {
	check := func(content string) {
		t.Helper()
		for _, glued := range []bool{false, true} {
			if got, want := qqCommandAsks(content, glued), previousQQCommandAsks(content, glued); got != want {
				t.Fatalf("问句前缀判断变化：内容=%q，连写=%t，结果=%t，期望=%t", content, glued, got, want)
			}
		}
	}
	for _, question := range []string{"怎么", "怎样", "如何", "为什么", "为啥", "吗", "呢", "吧", "在哪", "哪里", "哪儿", "什么", "是不是", "能不能", "可不可以", "不了", "不生效", "没用", "失败", "?", "？"} {
		for i := range len(question) + 1 {
			for _, head := range []string{"", " ", "\n", "\u3000", "FAQ ", "\xff", "\xe4\xb8"} {
				check(head + question[:i] + "正文怎么导入？")
			}
		}
	}
	for _, content := range []string{"", "后不生效怎么办", "了不起", "的格式", "条目", "功能", "库", "不够详细，没写怎么导入", strings.Repeat("详细说明", 500) + "怎么导入？"} {
		check(content)
	}
	random := rand.New(rand.NewSource(975))
	units := []string{"怎么", "哪里", "失败", "?", "？", "可不可以", "不", "了", "的", "条", "功能", "库", "后", "文档", "FAQ", " ", "\t", "\n", "\r", "\u3000", "\ufffd", "\xff", "\xe4\xb8", "😀"}
	for range 3000 {
		var content strings.Builder
		for range random.Intn(40) {
			content.WriteString(units[random.Intn(len(units))])
		}
		check(content.String())
		raw := make([]byte, random.Intn(100))
		_, _ = random.Read(raw)
		check(string(raw))
	}
}

func TestQQCommandAsksAvoidsMatchAllocation(t *testing.T) {
	for _, content := range []string{"怎么用", "？", "可不可以添加", "后不生效怎么办", "FAQ 没写怎么导入"} {
		var got bool
		allocations := testing.AllocsPerRun(100, func() { got = qqCommandAsks(content, true) })
		if want := previousQQCommandAsks(content, true); got != want {
			t.Fatalf("内容=%q，结果=%t，期望=%t", content, got, want)
		}
		if allocations != 0 {
			t.Errorf("内容=%q，分配 %g 次，期望零分配", content, allocations)
		}
	}
}

func BenchmarkQQCommandAsksPrefix(b *testing.B) {
	for _, tc := range []struct {
		name, content string
		glued         bool
	}{
		{"短问句", "怎么用", false},
		{"短普通正文", "FAQ 需要补充示例", false},
		{"短尾部问句", "FAQ 没写怎么导入", false},
		{"连写长正文", "的" + strings.Repeat("详细说明", 500), true},
		{"长普通正文", strings.Repeat("详细说明", 500), false},
		{"长尾部问句", strings.Repeat("详细说明", 500) + "怎么导入？", false},
		{"无效UTF8", "\xff" + strings.Repeat("详细说明", 500) + "怎么导入？", false},
	} {
		for _, impl := range []struct {
			name string
			asks func(string, bool) bool
		}{
			{"Previous", previousQQCommandAsks}, {"Current", qqCommandAsks},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.asks(tc.content, tc.glued)
				}
			})
		}
	}
}
