package server

import (
	"log/slog"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestQQNormalizeBlankLinesAllocationBudget(t *testing.T) {
	for _, sample := range []struct {
		name, text, want string
		limit            float64
	}{
		{"short", "第一段\n\n\n\n第二段", "第一段\n\n第二段", 1},
		// 避开含「文」字时触发的提示词正则，其缓存池在竞态模式下会随机丢弃对象。
		{"dense", strings.Repeat("回答\n\n\n", 512), strings.Repeat("回答\n\n", 512), 1},
		{"only_newlines", strings.Repeat("\n", 1<<18), "\n\n", 0},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(20, func() { got = qqNormalizeAnswer(sample.text) })
			if got != sample.want {
				t.Fatalf("连续换行压缩改变答案：实际=%q，预期=%q", got, sample.want)
			}
			t.Logf("答案连续换行压缩分配 %.0f 次", allocations)
			if allocations > sample.limit {
				t.Fatalf("连续换行压缩分配 %.0f 次，预算 %.0f 次，应直接跳过多余换行", allocations, sample.limit)
			}
		})
	}
}

func qqBlankLineSamples() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"short", "第一段\n\n\n\n第二段"},
		{"dense", strings.Repeat("正文\n\n\n", 512)},
		{"only_newlines", strings.Repeat("\n", 1<<18)},
		{"small_output", "前" + strings.Repeat("\n", 1<<18) + "后"},
		{"late", strings.Repeat("普通回答。", 4096) + "\n\n\n尾"},
		{"double_newlines", strings.Repeat("正文\n\n", 512)},
		{"plain", strings.Repeat("普通回答。", 4096)},
	}
}

func BenchmarkQQNormalizeBlankLinesFlow(b *testing.B) {
	for _, sample := range qqBlankLineSamples() {
		b.Run(sample.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqNormalizeAnswer(sample.text)
			}
		})
	}
}

// 冻结旧表达式和前置条件，独立校验连续换行压缩的输出。
var previousQQBlankLinesPattern = regexp.MustCompile(`\n{3,}`)

func previousQQCollapseBlankLines(text string) string {
	if strings.Contains(text, "\n\n\n") {
		return previousQQBlankLinesPattern.ReplaceAllString(text, "\n\n")
	}
	return text
}

func checkQQBlankLinesCompatibility(t testing.TB, text string) {
	t.Helper()
	if got, want := qqCollapseBlankLines(text), previousQQCollapseBlankLines(text); got != want {
		t.Fatalf("连续换行扫描改变文本：输入=%q，实际=%q，预期=%q", text, got, want)
	}
}

func TestQQBlankLinesScanCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "\n", "\n\n", "\n\n\n", "前\n\n\n后", "\n\n\n前\n后\n\n\n\n",
		"\r\n\r\n\r\n", "\n\n\r\n\n", "\n\n\t\n\n", "\n\n \n\n", "\n\n\v\n\n", "\n\n\f\n\n",
		"\u2028\u2028\u2028", "\n\n\u2028\n\n", "\xff\n\n\n\xfe", "\n\n\x00\n\n", "\xe2\x80\n\n\n\xa8",
	} {
		checkQQBlankLinesCompatibility(t, text)
	}
	for count := range 65 {
		checkQQBlankLinesCompatibility(t, strings.Repeat("\n", count))
		checkQQBlankLinesCompatibility(t, strings.Repeat("甲"+strings.Repeat("\n", count)+"乙\n", 8))
	}
	for value := range 256 {
		c := string([]byte{byte(value)})
		for _, text := range []string{c + "\n\n\n", "\n" + c + "\n\n", "\n\n" + c + "\n", "\n\n\n" + c + "\n\n\n", "甲" + c + "\n\n\n乙"} {
			checkQQBlankLinesCompatibility(t, text)
		}
	}
	random := rand.New(rand.NewSource(1039))
	atoms := []string{"合成正文", "\n", "\n\n", "\n\n\n", "\r", "\t", " ", "\v", "\f", "\xff", "\xe2", "\x80", "\x00", "\u2028"}
	for range 5000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkQQBlankLinesCompatibility(t, text.String())
	}
	for range 2000 {
		data := make([]byte, random.Intn(256))
		for i := range data {
			data[i] = byte(random.Intn(256))
		}
		checkQQBlankLinesCompatibility(t, string(data))
	}
}

func TestQQBlankLinesScanNoMatchAllocationBudget(t *testing.T) {
	for _, text := range []string{"", "普通回答", strings.Repeat("普通正文\n\n", 4096), strings.Repeat("\r\n", 65536)} {
		var got string
		allocations := testing.AllocsPerRun(20, func() { got = qqCollapseBlankLines(text) })
		if got != text || allocations != 0 {
			t.Fatalf("无匹配正文应原样复用：相同=%v，分配=%.0f", got == text, allocations)
		}
	}
}

func TestQQBlankLinesStageAllocationBudget(t *testing.T) {
	text := strings.Repeat("正文\n\n\n", 512)
	want := strings.Repeat("正文\n\n", 512)
	var got string
	allocations := testing.AllocsPerRun(20, func() { got = qqCollapseBlankLines(text) })
	if got != want || allocations > 1 {
		t.Fatalf("换行压缩应只分配一个结果缓冲：输出正确=%v，分配=%.0f", got == want, allocations)
	}
}

func TestQQNormalizeBlankLinesStageOrder(t *testing.T) {
	for _, sample := range []struct{ text, want string }{
		{"甲\n#\n##\n乙", "甲\n\n乙"},
		{"甲\n**\n__\n乙", "甲\n\n乙"},
		{"甲\n\n\t\n\n乙", "甲\n\n \n\n乙"},
		{"甲\n\n\\t\n\n乙", "甲\n\n \n\n乙"},
		{"C:\\temp\n\n\\t\n\n乙", "C:\\temp\n\n\\t\n\n乙"},
		{"don’t\n\n\n“重试”", "don't\n\n「重试」"},
		{"甲\r\n\r\n\r\n乙", "甲\r\n\r\n\r\n乙"},
		{"\xff\n\n\n\xfe", "\xff\n\n\xfe"},
	} {
		if got := qqNormalizeAnswer(sample.text); got != sample.want {
			t.Fatalf("答案格式处理顺序变化：输入=%q，实际=%q，预期=%q", sample.text, got, sample.want)
		}
	}
}

func TestQQBlankLinesKeepsSmallOutput(t *testing.T) {
	text := "前" + strings.Repeat("\n", 1<<18) + "后"
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqCollapseBlankLines(text)
		}
	})
	if got != "前\n\n后" {
		t.Fatalf("长换行串改变正文：%q", got)
	}
	if result.AllocedBytesPerOp() > 64 {
		t.Fatalf("短结果分配 %d 字节，不应按输入长度预留输出缓冲", result.AllocedBytesPerOp())
	}
}

func BenchmarkQQBlankLinesScan(b *testing.B) {
	for _, sample := range qqBlankLineSamples() {
		for _, implementation := range []struct {
			name     string
			collapse func(string) string
		}{
			{"previous", previousQQCollapseBlankLines},
			{"current", qqCollapseBlankLines},
		} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.collapse(sample.text)
				}
			})
		}
	}
}

func FuzzQQBlankLinesScan(f *testing.F) {
	for _, text := range []string{"", "\n", "\n\n\n", "\r\n\r\n\r\n", "甲\n\n\n乙", "\xff\n\n\n\xfe", "\n\n \n\n", strings.Repeat("甲\n\n\n", 9)} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) { checkQQBlankLinesCompatibility(t, text) })
}

// 冻结修改前的完整答案规范化流程，排除跨进程基准差异。
func previousQQNormalizeBlankLinesFlow(answer string) string {
	answer = qqNormalizeApostrophes(answer)
	candidates := qqScanAnswerCandidates(answer)
	promptLeak, replacement := candidates.promptLeak, candidates.replacement
	if promptLeak {
		if word := qqPromptLeak.FindString(answer); word != "" {
			slog.Info("qq bot: prompt leak", "word", word)
		}
	}
	// 这组含多字节引号的替换规则也会复制无匹配正文，缺少所有标记时直接复用。
	if replacement || strings.Contains(answer, "**") || strings.Contains(answer, "__") {
		answer = qqAnswerReplacer.Replace(answer)
	}
	if strings.Contains(answer, `\t`) && !qqWindowsPath.MatchString(answer) {
		answer = strings.ReplaceAll(answer, `\t`, " ")
	}
	if candidates.headingCandidate {
		answer = qqStripAnswerHeadings(answer)
	}
	if strings.Contains(answer, "\n\n\n") {
		answer = previousQQBlankLinesPattern.ReplaceAllString(answer, "\n\n")
	}
	return answer
}

func BenchmarkQQBlankLinesFlowComparison(b *testing.B) {
	for _, sample := range qqBlankLineSamples() {
		for _, implementation := range []struct {
			name      string
			normalize func(string) string
		}{
			{"previous", previousQQNormalizeBlankLinesFlow},
			{"current", qqNormalizeAnswer},
		} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.normalize(sample.text)
				}
			})
		}
	}
}

func TestQQBlankLinesFlowCompatibility(t *testing.T) {
	for _, sample := range qqBlankLineSamples() {
		if got, want := qqNormalizeAnswer(sample.text), previousQQNormalizeBlankLinesFlow(sample.text); got != want {
			t.Fatalf("完整规范化流程不兼容：样本=%s，实际=%q，预期=%q", sample.name, got, want)
		}
	}
}
