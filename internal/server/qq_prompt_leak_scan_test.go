package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestQQNormalizePromptLeakNoMatchAllocationBudget(t *testing.T) {
	for _, sample := range []struct{ name, text string }{
		{"ordinary_text", strings.Repeat("正文", 512)},
		{"separated_markers", strings.Repeat("资 料 文 档 里 截 断 ", 512)},
		{"late_marker", strings.Repeat("普通回答。", 4096) + "文"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(100, func() { got = qqNormalizeAnswer(sample.text) })
			if got != sample.text {
				t.Fatal("未命中提示词的答案发生变化")
			}
			t.Logf("未命中提示词的答案规范化分配 %.0f 次", allocations)
			if allocations != 0 {
				t.Fatalf("未命中提示词仍分配 %.0f 次，应原样复用答案并避免正则缓存分配", allocations)
			}
		})
	}
}

func qqPromptLeakFlowSamples() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"short", "请检查说明文字。"},
		{"ordinary_text", strings.Repeat("正文", 512)},
		{"separated_markers", strings.Repeat("资 料 文 档 里 截 断 ", 512)},
		{"late_marker", strings.Repeat("普通回答。", 4096) + "文"},
		{"plain", strings.Repeat("普通回答。", 4096)},
	}
}

func BenchmarkQQNormalizePromptLeakFlow(b *testing.B) {
	for _, sample := range qqPromptLeakFlowSamples() {
		b.Run(sample.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqNormalizeAnswer(sample.text)
			}
		})
	}
}

var previousQQPromptLeakPattern = regexp.MustCompile(`资料|文档里|截断`)

func TestQQPromptLeakWordCompatibility(t *testing.T) {
	inputs := []string{"", "普通回答", "资料里", "文档里", "文档说明", "已截断", "截断资料", "资料和文档里", "\xff资料", "文档里\xfe"}
	random := rand.New(rand.NewSource(1040))
	atoms := []string{"普通回答", "资", "料", "文", "档", "里", "截", "断", "\n", "\xff", "\x00"}
	for range 5000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		inputs = append(inputs, text.String())
	}
	for _, input := range inputs {
		if got, want := qqPromptLeak.FindString(input), previousQQPromptLeakPattern.FindString(input); got != want {
			t.Fatalf("提示词日志匹配变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQPromptLeakScanNoMatchAllocationBudget(t *testing.T) {
	for _, text := range []string{"", "普通回答", strings.Repeat("文档说明", 4096), strings.Repeat("资 料 ", 4096)} {
		var got string
		allocations := testing.AllocsPerRun(100, func() { got = qqPromptLeak.FindString(text) })
		if got != previousQQPromptLeakPattern.FindString(text) || allocations != 0 {
			t.Fatalf("提示词日志扫描应原样复用：结果=%q，分配=%.0f", got, allocations)
		}
	}
}

func FuzzQQPromptLeakWord(f *testing.F) {
	for _, text := range []string{"", "资料", "文档里", "文档说明", "截断", "普通回答", "\xff资料", "文档里\xfe"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if got, want := qqPromptLeak.FindString(text), previousQQPromptLeakPattern.FindString(text); got != want {
			t.Fatalf("提示词日志匹配变化：输入=%q，实际=%q，预期=%q", text, got, want)
		}
	})
}
