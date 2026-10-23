package server

import (
	"math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

var qqHeadingPattern = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

func markdownHeadingTailInput(headings int, padding string) string {
	return "# 合成文档\n" + strings.Repeat("## 合成标题\n正文\n", headings) + padding
}

func TestQQMarkdownHeadingTailCompatibility(t *testing.T) {
	texts := []string{
		"", "## 标题", "## 标题\n", "## 标题\n\xff", "## 标题\n\x00", "## 标题\n\xe2\x80",
		"# 文档\n导语\n## 一\n正文\n### 二\n", "## 一\r\n正文\r\n### 二\r\n",
		"## 一\n```go\n## 代码中的标题\n```\n## 二\n正文\n",
		"## 一\n" + strings.Repeat("字", qqSectionRunes+10) + "\n## 二\n短正文\n",
		markdownHeadingTailInput(64, ""),
	}
	random := rand.New(rand.NewSource(1066))
	units := []string{"# 文档\n", "## 标题\n", "### 子标题\n", "正文\n", "\n", "```\n", " \t\n", "\u0085\u205f\n", "\xff\n", "## \n", "\r\n"}
	for range 100 {
		var text strings.Builder
		for range random.Intn(40) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		for _, tail := range []string{"", " \t\r\n", "\u0085\u00a0\u1680\u2000\u2009\u2028\u2029\u202f\u205f\u3000", "\u2009\xff\u2009", "\u2009\x00\u2009"} {
			markdown := text + tail
			if got, want := qqSplitMarkdown("synthetic", markdown), previousQQSplitMarkdownHeadingTail("synthetic", markdown); !slices.Equal(got, want) {
				t.Fatalf("标题尾部判断改变分段：输入=%q，结果=%+v，期望=%+v", markdown, got, want)
			}
		}
	}
}

func TestQQMarkdownHeadingTailScanBudget(t *testing.T) {
	text := markdownHeadingTailInput(256, strings.Repeat("\u2009", 1<<14))
	want := previousQQSplitMarkdownHeadingTail("synthetic", text)
	baseline := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_ = previousQQSplitMarkdownHeadingTail("synthetic", text)
		}
	})
	var got []qqSection
	current := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqSplitMarkdown("synthetic", text)
		}
	})
	if !slices.Equal(got, want) || len(got) != 256 {
		t.Fatal("含长尾部空白的多标题文档分段结果变化")
	}
	if current.NsPerOp() >= baseline.NsPerOp()/4 {
		t.Fatalf("多标题解析耗时 %d ns，未低于旧尾部扫描 %d ns 的四分之一", current.NsPerOp(), baseline.NsPerOp())
	}
}

func TestQQMarkdownHeadingScanAllocationBudget(t *testing.T) {
	text := markdownHeadingTailInput(256, "")
	allocations := testing.AllocsPerRun(20, func() {
		if got := qqSplitMarkdown("synthetic", text); len(got) != 256 {
			t.Fatalf("分段数=%d，期望 256", len(got))
		}
	})
	if allocations > 800 {
		t.Fatalf("多标题解析分配 %.0f 次，超过 800 次预算", allocations)
	}
}

func BenchmarkQQMarkdownHeadingTail(b *testing.B) {
	for _, sample := range []struct {
		name, text string
	}{
		{"short", "# 文档\n## 标题\n正文"},
		{"ordinary", markdownHeadingTailInput(256, "")},
		{"ascii_tail", markdownHeadingTailInput(256, strings.Repeat(" \t\n", 1<<14))},
		{"unicode_tail", markdownHeadingTailInput(256, strings.Repeat("\u2009", 1<<14))},
	} {
		for i, impl := range []func(string, string) []qqSection{previousQQSplitMarkdownHeadingTail, qqSplitMarkdown} {
			b.Run(sample.name+"/"+[]string{"previous", "current"}[i], func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl("synthetic", sample.text)
				}
			})
		}
	}
	for _, count := range []int{32, 256} {
		text := markdownHeadingTailInput(count, strings.Repeat("\u2009", 1<<14))
		b.Run("scaling/"+strconv.Itoa(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSplitMarkdown("synthetic", text)
			}
		})
	}
}

// previousQQSplitMarkdownHeadingTail 保留逐标题裁剪整个后缀的旧实现，核对分段语义和耗时。
func previousQQSplitMarkdownHeadingTail(name, markdown string) []qqSection {
	doc := name
	var sections []qqSection
	var titles []string
	var body strings.Builder
	bodyRunes, bodyTruncated := 0, false
	fenced, intro := false, false
	appendBody := func(line, leading string) {
		if bodyTruncated {
			return
		}
		if line == "" {
			if bodyRunes > 0 && bodyRunes < qqSectionRunes {
				body.WriteByte('\n')
				bodyRunes++
			}
			return
		}
		// 正文足够长时一次扩到上限，避免逐行追加造成多次扩容；短段落保留小缓冲。
		if bodyRunes > 256 && body.Cap() < qqSectionRunes*utf8.UTFMax {
			body.Grow(qqSectionRunes*utf8.UTFMax - body.Cap())
		}
		if bodyRunes == 0 {
			line = leading
		}
		end := 0
		if bodyRunes < qqSectionRunes {
			end = len(line)
			for i := range line {
				if bodyRunes == qqSectionRunes {
					end = i
					break
				}
				bodyRunes++
			}
			body.WriteString(line[:end])
		}
		// 上限之后只有空白时，仍需按未截断正文去掉尾部空白。
		if qqTrimSpace(line[end:]) != "" {
			bodyTruncated = true
		}
		if bodyRunes > 0 && bodyRunes < qqSectionRunes {
			body.WriteByte('\n')
			bodyRunes++
		}
	}
	flush := func() {
		capacity := body.Cap()
		text, truncated := body.String(), bodyTruncated
		if !truncated {
			text = qqTrimSpace(text)
		}
		body.Reset()
		bodyRunes, bodyTruncated = 0, false
		title := strings.Join(titles, " > ")
		if len(titles) == 0 && intro {
			title = "概述"
		}
		if text == "" || title == "" {
			return
		}
		if truncated && !utf8.ValidString(text) {
			// 沿用截断正文的编码语义，未截断正文仍保留原始字节。
			text = string([]rune(text))
		} else if capacity > qqSectionRunes*utf8.UTFMax && len(text) < capacity/2 {
			// 缓冲超过段落允许的最大字节数且大半为空白时，复制正文释放空白。
			text = strings.Clone(text)
		}
		sections = append(sections, qqSection{Title: title, Text: text})
	}
	for offset := 0; offset < len(markdown); {
		lineEnd := strings.IndexByte(markdown[offset:], '\n')
		nextOffset := len(markdown)
		line := markdown[offset:]
		if lineEnd >= 0 {
			lineEnd += offset
			nextOffset = lineEnd + 1
			line = markdown[offset:lineEnd]
			// 只处理有 LF 的行，保留文件末尾未组成 CRLF 的孤立回车。
			line = strings.TrimSuffix(line, "\r")
		}
		// 代码块检测与段落开头裁剪复用前导空白扫描，后续正文仍保留缩进。
		leading := qqMarkdownLeadingSpace(line)
		if strings.HasPrefix(leading, "```") {
			fenced = !fenced
		}
		var m []string
		if !fenced && strings.HasPrefix(line, "#") {
			m = qqHeadingPattern.FindStringSubmatch(line)
		}
		if m != nil {
			flush()
			if len(m[1]) > 1 && qqTrimSpace(markdown[nextOffset:]) == "" {
				break
			}
			level := len(m[1])
			// 标题随段落进入缓存，复制捕获结果以免引用整份 Markdown 的底层内存。
			heading := strings.Clone(m[2])
			if level == 1 {
				doc, titles, intro = heading, nil, true
				offset = nextOffset
				continue
			}
			// 二级标题是第一层，更深的标题接在上一层后面。
			depth := level - 2
			if depth > len(titles) {
				depth = len(titles)
			}
			titles = append(titles[:depth], heading)
			offset = nextOffset
			continue
		}
		if len(titles) > 0 || intro {
			appendBody(line, leading)
		}
		offset = nextOffset
	}
	flush()
	for i := range sections {
		sections[i].Doc = doc
	}
	return sections
}
