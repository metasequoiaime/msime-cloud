package server

import (
	"math/rand"
	"strings"
	"testing"
)

func previousQQWebSearchText(content []qqWebSearchContent) string {
	var texts []string
	for _, c := range content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			texts = append(texts, strings.TrimSpace(c.Text))
		}
	}
	if len(texts) == 0 {
		return ""
	}
	return qqTruncate(strings.Join(texts, "\n"), qqResearchResultRunes)
}

func TestQQWebSearchTextMatchesPrevious(t *testing.T) {
	inputs := [][]qqWebSearchContent{
		{},
		{{Type: "tool", Text: "ignored"}},
		{{Type: "text", Text: "  first  "}},
		{{Type: "text", Text: " first "}, {Type: "text", Text: "\tsecond\n"}},
		{{Type: "text", Text: "  "}, {Type: "tool", Text: "ignored"}, {Type: "text", Text: "third"}},
	}
	random := rand.New(rand.NewSource(1112))
	for range 1000 {
		content := make([]qqWebSearchContent, random.Intn(20))
		for i := range content {
			content[i] = qqWebSearchContent{Type: "text", Text: strings.Repeat(" ", random.Intn(3)) + "结果" + strings.Repeat("\t", random.Intn(2))}
			if random.Intn(4) == 0 {
				content[i].Type = "tool"
			}
		}
		inputs = append(inputs, content)
	}
	for _, content := range inputs {
		if got, want := qqJoinWebSearchText(content), previousQQWebSearchText(content); got != want {
			t.Fatalf("联网搜索结果变化：输入=%#v，结果=%q，期望=%q", content, got, want)
		}
	}
}

func TestQQWebSearchTextAllocationBudget(t *testing.T) {
	content := make([]qqWebSearchContent, 20)
	for i := range content {
		content[i] = qqWebSearchContent{Type: "text", Text: strings.Repeat("结果 ", 100)}
	}
	allocations := testing.AllocsPerRun(20, func() {
		_ = qqJoinWebSearchText(content)
	})
	if allocations > 2 {
		t.Fatalf("联网搜索结果整理分配 %.1f 次，超过 2 次预算", allocations)
	}
}

func BenchmarkQQWebSearchText(b *testing.B) {
	content := make([]qqWebSearchContent, 20)
	for i := range content {
		content[i] = qqWebSearchContent{Type: "text", Text: strings.Repeat("结果 ", 100)}
	}
	for _, current := range []struct {
		name string
		join func([]qqWebSearchContent) string
	}{
		{"previous", previousQQWebSearchText},
		{"current", qqJoinWebSearchText},
	} {
		b.Run(current.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = current.join(content)
			}
		})
	}
}
