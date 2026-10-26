package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestQQPlainTextRawLinkAllocationBudget(t *testing.T) {
	for _, sample := range []struct {
		name, text, want string
		limit            float64
	}{
		{"dense", strings.Repeat("普通回答 https://example.test/path ", 512), strings.TrimSpace(strings.Repeat("普通回答 ", 512)), 3},
		{"long_target", "前 " + strings.Repeat("https://example.test/path/", 1<<15) + " 后", "前 后", 3},
		{"no_link_marker", strings.Repeat("普通回答。", 32768), strings.Repeat("普通回答。", 32768), 0},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var got string
			allocations := testing.AllocsPerRun(20, func() { got = qqPlainText(sample.text) })
			if got != sample.want {
				t.Fatalf("裸链接过滤结果变化：实际=%q，预期=%q", got, sample.want)
			}
			t.Logf("裸链接纯文本分配 %.0f 次", allocations)
			if allocations > sample.limit {
				t.Fatalf("裸链接纯文本分配 %.0f 次，预算 %.0f 次，应避免正则捕获和中间正文复制", allocations, sample.limit)
			}
		})
	}
}

func TestQQPlainTextRawLinkKeepsSmallOutputBuffer(t *testing.T) {
	text := "前 " + strings.Repeat("https://example.test/path/", 1<<15) + " 后"
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if got := qqPlainText(text); got != "前 后" {
				b.Fatal("长裸链接结果变化")
			}
		}
	})
	if result.AllocedBytesPerOp() > 64<<10 {
		t.Fatalf("长裸链接结果分配 %d 字节，不应按链接目标长度预留缓冲", result.AllocedBytesPerOp())
	}
}

func qqPlainLinkSamples() []struct{ name, text string } {
	return []struct{ name, text string }{
		{"short", "请看 https://example.test/settings。"},
		{"dense", strings.Repeat("普通回答 https://example.test/path ", 512)},
		{"long_target", "前 " + strings.Repeat("https://example.test/path/", 1<<15) + " 后"},
		{"late", strings.Repeat("普通回答。", 4096) + " https://example.test/path"},
		{"plain", strings.Repeat("普通回答。", 4096)},
	}
}

func BenchmarkQQPlainTextRawLinkFlow(b *testing.B) {
	for _, sample := range qqPlainLinkSamples() {
		b.Run(sample.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(sample.text)
			}
		})
	}
}

var previousQQPlainLinkPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[!-~]*`)

func TestQQPlainLinkScanCompatibility(t *testing.T) {
	units := []string{"", "a", "1", "+", "-", ".", ":", "/", "!", " ", "甲", "\n", "\t", "\xff", "https", "ftp", "普通回答", "**", "#"}
	for _, text := range []string{
		"", "https://example.test", "HTTP+unix://socket/path", "7://not-a-scheme", "普通://网址", "前https://a.b后",
		"https://a.bhttp://c.d", "http://a.b/path?x=1#x", "[链接](https://a.b)", "https://\xff/路径",
	} {
		checkQQPlainLinkStage(t, text)
	}
	random := rand.New(rand.NewSource(1041))
	for range 5000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(units[random.Intn(len(units))])
		}
		checkQQPlainLinkStage(t, text.String())
	}
}

func checkQQPlainLinkStage(t testing.TB, text string) {
	t.Helper()
	want := text
	if strings.Contains(want, "://") {
		want = previousQQPlainLinkPattern.ReplaceAllString(want, "")
	}
	if got := qqRemoveLinks(text); got != qqCollapseSpaceRuns(want) {
		t.Fatalf("裸链接扫描变化：输入=%q，实际=%q，预期=%q", text, got, qqCollapseSpaceRuns(want))
	}
}

func TestQQPlainTextRawLinkStageOrder(t *testing.T) {
	for _, sample := range []struct{ text, want string }{
		{"正文 https://example.test **加粗**", "正文 加粗"},
		{"https://example.test\n# 标题", "标题"},
		{"前 https://a.b 后\n下一行", "前 后 下一行"},
		{"https://a.b[链接](x)", "链接"},
		{"https://a.b`代码`", "代码"},
		{"普通://网址", "普通://网址"},
	} {
		if got := qqPlainText(sample.text); got != sample.want {
			t.Fatalf("裸链接处理顺序变化：输入=%q，实际=%q，预期=%q", sample.text, got, sample.want)
		}
	}
}

func BenchmarkQQPlainLinkScan(b *testing.B) {
	for _, sample := range qqPlainLinkSamples() {
		for _, implementation := range []struct {
			name  string
			strip func(string) string
		}{
			{"previous", func(text string) string { return previousQQPlainLinkPattern.ReplaceAllString(text, "") }},
			{"current", qqRemoveLinks},
		} {
			b.Run(sample.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.strip(sample.text)
				}
			})
		}
	}
}

func FuzzQQPlainLinkScan(f *testing.F) {
	for _, text := range []string{"", "https://example.test", "HTTP+unix://socket/path", "7://not-a-scheme", "普通://网址", "前https://a.b后", "https://a.bhttp://c.d", "\xffhttps://a.b\xfe"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) { checkQQPlainLinkStage(t, text) })
}
