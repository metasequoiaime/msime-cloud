package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// 独立保留旧表达式及条件替换路径，用于校验重叠、跨行和无效 UTF-8 的原有行为。
var previousQQImageHTMLPattern = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|<[^>\n]+>`)

func previousQQStripImagesAndHTML(text string) string {
	start := strings.Index(text, "![")
	if tag := strings.IndexByte(text, '<'); tag >= 0 && (start < 0 || tag < start) {
		start = tag
	}
	if start < 0 {
		return text
	}
	candidate := text[start:]
	if start >= len(candidate) {
		match := previousQQImageHTMLPattern.FindStringIndex(candidate)
		if match == nil {
			return text
		}
		tail := candidate[match[1]:]
		if !strings.Contains(tail, "![") && !strings.ContainsRune(tail, '<') {
			return text[:start+match[0]] + " " + tail
		}
		return text[:start] + previousQQImageHTMLPattern.ReplaceAllString(candidate, " ")
	}
	if !previousQQImageHTMLPattern.MatchString(candidate) {
		return text
	}
	return previousQQImageHTMLPattern.ReplaceAllString(text, " ")
}

func checkQQImageHTMLScan(t testing.TB, text string) {
	t.Helper()
	want := previousQQImageHTMLPattern.ReplaceAllString(text, " ")
	if got := qqStripImagesAndHTML(text); got != want {
		t.Fatalf("图片及标签过滤变化：输入=%q，实际=%q，预期=%q", text, got, want)
	}
	expected := previousQQImageHTMLPattern.FindAllStringIndex(text, -1)
	index := 0
	qqScanImageAndHTML(text, func(start, end int) {
		if index >= len(expected) || start != expected[index][0] || end != expected[index][1] {
			t.Fatalf("匹配下标变化：输入=%q，序号=%d，实际=%d/%d，预期=%v", text, index, start, end, expected)
		}
		index++
	})
	if index != len(expected) {
		t.Fatalf("匹配数量变化：实际=%d，预期=%d", index, len(expected))
	}
}

func TestQQImageHTMLScanCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "正文", "![]()", "![a](x)", "![a\nb](x\ny)", "<> <\n> <\r> <<>>",
		"![外![内](x)", "![外![内]失效 ![后](x)", "![a<b>](x)", "<a![b](x)>",
		"![<b>无右括号", "![<b>]无左括号", "<无闭合 ![a](x)", "<跨\n行>![a](x)",
		"![a](<b>)<c>", "<![a]()>尾", "![a](x)y)尾", "![a]](x) ![b](y)",
		"![a](x)![b](y)", "<a><b></b></a>", "![\xff](\xfe) <\xff> \xfe", "![a\x00](x) <\x00>",
	} {
		checkQQImageHTMLScan(t, text)
	}
	for _, count := range []int{7, 8, 9, 16} {
		checkQQImageHTMLScan(t, strings.Repeat("甲<b>乙</b>![图](x)", count))
	}
	for value := range 256 {
		c := string([]byte{byte(value)})
		for _, text := range []string{"![" + c + "](x)", "![a](" + c + ")", "<" + c + ">", c + "![a](x)<b>", "![a]" + c + "(x)<b>"} {
			checkQQImageHTMLScan(t, text)
		}
	}
	random := rand.New(rand.NewSource(1036))
	atoms := []string{"正文", "!", "[", "]", "(", ")", "<", ">", "\n", "\r", "\t", "\x00", "\xff", "\xfe", "![a](x)", "![", "](", "<b>", "</b>", "<>"}
	for range 6000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(atoms[random.Intn(len(atoms))])
		}
		checkQQImageHTMLScan(t, text.String())
	}
}

func qqImageHTMLSamples() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"short", "甲<b>乙</b>![截图](https://example.test/a.png)丙"},
		{"plain", strings.Repeat("普通正文。", 4096)},
		{"dense", strings.Repeat("甲<b>乙</b>![图](x)", 512)},
		{"late", strings.Repeat("普通正文。", 4096) + "<b>尾</b>"},
		{"unclosed_images", strings.Repeat("![未闭合", 4096)},
		{"invalid_images", strings.Repeat("![嵌套![x]无左括号 ", 2048)},
		{"unclosed_tags", strings.Repeat("<tag synthetic\n", 4096)},
		{"nested_openers", strings.Repeat("![<", 4096) + "](x)>"},
		{"images_inside_tags", strings.Repeat("<![x>", 4096) + "](x)"},
		{"tags_inside_images", strings.Repeat("![<](x)", 4096) + ">"},
	}
}

func TestQQImageHTMLScanAllocationBudget(t *testing.T) {
	for _, sample := range qqImageHTMLSamples()[:4] {
		t.Run(sample.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(50, func() { got = qqStripImagesAndHTML(sample.text) })
			want := previousQQImageHTMLPattern.ReplaceAllString(sample.text, " ")
			if got != want {
				t.Fatal("过滤输出变化")
			}
			limit := float64(1)
			if sample.name == "plain" {
				limit = 0
			}
			t.Logf("分配次数：%.0f", allocations)
			if allocations > limit {
				t.Fatalf("过滤产生 %.0f 次分配，预期最多 %.0f 次，只构建保留的正文", allocations, limit)
			}
		})
	}
}

func TestQQImageHTMLScanTimeBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("竞态模式只验证兼容性和分配，相对耗时在普通模式检查")
	}
	for _, sample := range qqImageHTMLSamples()[2:] {
		// 末尾少量标记的旧实现已有前缀优化，只检查单次分配；扫描加速预算用于压力样本。
		if sample.name == "late" {
			continue
		}
		t.Run(sample.name, func(t *testing.T) {
			baseline := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = previousQQStripImagesAndHTML(sample.text)
				}
			})
			current := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = qqStripImagesAndHTML(sample.text)
				}
			})
			checkQQImageHTMLScan(t, sample.text)
			t.Logf("旧实现：%s；当前：%s", baseline, current)
			if current.NsPerOp() >= baseline.NsPerOp()/2 {
				t.Fatalf("仍承担重复正则扫描开销：旧=%s，新=%s", baseline, current)
			}
		})
	}
}

func TestQQImageHTMLScanRetainsOnlyOutput(t *testing.T) {
	text := "前![" + strings.Repeat("合成图片描述", 32768) + "](x)后"
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqStripImagesAndHTML(text)
		}
	})
	if got != "前 后" {
		t.Fatalf("长图片过滤结果=%q", got)
	}
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatalf("短输出分配 %d 字节，不应按被删除的正文预留空间", result.AllocedBytesPerOp())
	}
}

func BenchmarkQQImageHTMLScan(b *testing.B) {
	for _, sample := range qqImageHTMLSamples() {
		for _, implementation := range []struct {
			name   string
			filter func(string) string
		}{{"previous", previousQQStripImagesAndHTML}, {"current", qqStripImagesAndHTML}} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.filter(sample.text)
				}
			})
		}
	}
}

func FuzzQQImageHTMLScan(f *testing.F) {
	for _, sample := range qqImageHTMLSamples() {
		f.Add(sample.text)
	}
	f.Add("![<b>]失效 ![a](<c>)<\n>\xff")
	f.Fuzz(func(t *testing.T, text string) { checkQQImageHTMLScan(t, text) })
}
