package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestQQPlainTextMarkdownLinkAllocationBudget(t *testing.T) {
	for _, sample := range []struct {
		name, text, want string
		limit            float64
	}{
		{"dense", strings.Repeat("[甲](https://example.test/path) ", 512), strings.TrimSpace(strings.Repeat("甲 ", 512)), 2},
		{"no_open", strings.Repeat("](", 8192), strings.Repeat("](", 8192), 0},
		{"no_close", strings.Repeat("[甲](", 4096), strings.Repeat("[甲](", 4096), 0},
		{"empty_labels", strings.Repeat("![](x)", 512), "", 0},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(20, func() { got = qqPlainText(sample.text) })
			if got != sample.want {
				t.Fatalf("纯文本输出变化：实际=%q，预期=%q", got, sample.want)
			}
			t.Logf("Markdown 纯文本分配 %.0f 次", allocations)
			if allocations > sample.limit {
				t.Fatalf("Markdown 纯文本分配 %.0f 次，预算 %.0f 次，应直接读取链接文字并复用无匹配正文", allocations, sample.limit)
			}
		})
	}
}

func qqMarkdownLinkSamples() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"short", "请看[设置方法](https://example.test/settings)。"},
		{"dense", strings.Repeat("[甲](https://example.test/path) ", 512)},
		{"no_open", strings.Repeat("](", 8192)},
		{"no_close", strings.Repeat("[甲](", 4096)},
		{"empty_labels", strings.Repeat("![](x)", 512)},
		{"long_target", "前 [链接](" + strings.Repeat("synthetic", 32768) + ") 后"},
		{"late", strings.Repeat("普通正文。", 4096) + "[尾](x)"},
		{"plain", strings.Repeat("普通正文。", 4096)},
	}
}

func BenchmarkQQPlainTextMarkdownLinkFlow(b *testing.B) {
	for _, sample := range qqMarkdownLinkSamples() {
		b.Run(sample.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(sample.text)
			}
		})
	}
}

// 冻结旧表达式，分别校验每个捕获下标和最终替换文本。
var previousQQMarkdownLinkPattern = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)

func checkQQMarkdownLinkScan(t testing.TB, text string) {
	t.Helper()
	want := previousQQMarkdownLinkPattern.ReplaceAllString(text, "$1")
	if got := qqMarkdownLinksToText(text); got != want {
		t.Fatalf("链接文字变化：输入=%q，实际=%q，预期=%q", text, got, want)
	}
	matches := previousQQMarkdownLinkPattern.FindAllStringSubmatchIndex(text, -1)
	index := 0
	qqScanMarkdownLinks(text, func(match [4]int) {
		if index >= len(matches) {
			t.Fatalf("出现额外匹配：%q，%v", text, match)
		}
		for i := range match {
			if match[i] != matches[index][i] {
				t.Fatalf("链接匹配下标变化：输入=%q，序号=%d，实际=%v，预期=%v", text, index, match, matches[index])
			}
		}
		index++
	})
	if index != len(matches) {
		t.Fatalf("链接匹配数量变化：实际=%d，预期=%d", index, len(matches))
	}
}

func TestQQMarkdownLinkScanCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "[]()", "![]()", "!![x](y)", "![![x](y)", "[[x](y)](z)",
		"[x](a(b)c)", "[x] (y)", "[x]失效 [y](z)", "[外[内]失效 [后](x)",
		"[x](y)[a](b)", "[x](y)![a](b)", "[x\ny](a\nb)", "[\xff](\xfe)\xff", "[\x00](\x00)",
		"[x](未闭合 [y](后续)", strings.Repeat("[", 8192) + "]无目标", strings.Repeat("[", 8192) + "](x)",
	} {
		checkQQMarkdownLinkScan(t, text)
	}
	for _, count := range []int{7, 8, 9, 16} {
		checkQQMarkdownLinkScan(t, strings.Repeat("![甲](x)[乙](y)", count))
	}
	for value := range 256 {
		c := string([]byte{byte(value)})
		for _, text := range []string{c + "[x](y)", "[" + c + "](y)", "[x](" + c + ")", "[x]" + c + "(y)", "!" + c + "[x](y)"} {
			checkQQMarkdownLinkScan(t, text)
		}
	}
	random := rand.New(rand.NewSource(1037))
	atoms := []string{"合成正文", "!", "[", "]", "(", ")", "](", "\n", "\r", "\t", "\xff", "\xfe", "[甲](x)", "![乙](y)", "\x00"}
	for range 5000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkQQMarkdownLinkScan(t, text.String())
	}
	for range 2000 {
		data := make([]byte, random.Intn(256))
		for i := range data {
			data[i] = byte(random.Intn(256))
		}
		checkQQMarkdownLinkScan(t, string(data))
	}
}

func TestQQPlainTextMarkdownLinkStageOrder(t *testing.T) {
	for _, sample := range []struct{ text, want string }{
		{"## [标题](x)\n正文", "标题 正文"},
		{"```text\n[代码](x)\n```\n[正文](y)", "正文"},
		{"| [表格](x) |\n[正文](y)", "正文"},
		{"[**加粗**](x) [__强调__](y)", "加粗 强调"},
		{"[甲\n乙](x)", "甲 乙"},
		{"[https://example.test](x) 末尾", "末尾"},
		{"[[甲](x)](y)", "[甲](y)"},
		{"![甲](x) !![乙](y)", "甲 !乙"},
		{"[\xff](x)\xfe", "\xff\xfe"},
	} {
		if got := qqPlainText(sample.text); got != sample.want {
			t.Fatalf("链接与其他纯文本阶段的处理顺序变化：输入=%q，实际=%q，预期=%q", sample.text, got, sample.want)
		}
	}
}

func TestQQMarkdownLinkScanTimeBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("竞态模式验证输出和分配，普通模式验证相对扫描耗时")
	}
	for _, sample := range qqMarkdownLinkSamples()[1:6] {
		t.Run(sample.name, func(t *testing.T) {
			baseline := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = previousQQMarkdownLinkPattern.ReplaceAllString(sample.text, "$1")
				}
			})
			current := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = qqMarkdownLinksToText(sample.text)
				}
			})
			checkQQMarkdownLinkScan(t, sample.text)
			t.Logf("旧替换：%s；当前：%s", baseline, current)
			if current.NsPerOp() >= baseline.NsPerOp()/2 {
				t.Fatalf("链接文字替换仍承担全文正则开销：旧=%s，新=%s", baseline, current)
			}
		})
	}
}

func TestQQMarkdownLinkKeepsSmallOutput(t *testing.T) {
	text := "前 [链接](" + strings.Repeat("synthetic", 32768) + ") 后"
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqPlainText(text)
		}
	})
	if got != "前 链接 后" {
		t.Fatalf("长链接影响正文：%q", got)
	}
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatalf("短结果分配 %d 字节，不应按目标地址长度预留输出缓冲", result.AllocedBytesPerOp())
	}
}

func BenchmarkQQMarkdownLinkRewrite(b *testing.B) {
	for _, sample := range qqMarkdownLinkSamples()[:7] {
		for _, implementation := range []struct {
			name    string
			rewrite func(string) string
		}{
			{"previous", func(text string) string { return previousQQMarkdownLinkPattern.ReplaceAllString(text, "$1") }},
			{"current", qqMarkdownLinksToText},
		} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.rewrite(sample.text)
				}
			})
		}
	}
}

func FuzzQQMarkdownLinkScan(f *testing.F) {
	for _, text := range []string{"", "[x](y)", "!![x](y)", "[[x](y)](z)", "[x\ny](a\nb)", "![\xff](\xfe)", "[失效] [有效](x)", "[无闭合", "](无起点)", strings.Repeat("[甲](x)", 9)} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) { checkQQMarkdownLinkScan(t, text) })
}
