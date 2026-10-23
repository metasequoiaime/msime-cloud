package server

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func previousQQFAQCategories(faq string) []string {
	var categories []string
	for _, line := range strings.Split(faq, "\n") {
		if strings.HasPrefix(line, "## ") {
			categories = append(categories, strings.TrimSpace(line[3:]))
		}
	}
	return categories
}

func previousQQInsertFAQ(faq string, index int, question, answer, footer string) (string, bool) {
	lines := strings.Split(faq, "\n")
	seen, start := -1, -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			seen++
			if seen == index {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return faq, false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") || strings.HasPrefix(lines[i], footer) {
			end = i
			break
		}
	}
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	entry := []string{"", "### " + question, "", strings.TrimSpace(answer)}
	if end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		entry = append(entry, "")
	}
	out := append(append(append([]string{}, lines[:end]...), entry...), lines[end:]...)
	return strings.Join(out, "\n"), true
}

func TestQQFAQStreamingMatchesPrevious(t *testing.T) {
	texts := []string{
		"", "\n", "## A", "## A\n", "## A\n\n", "## A\r\n\r\n",
		"## A\n## B", "## A\n正文\n## B\n", "## A\n正文\n\n## B\n",
		"## A\n \t\r\n\u3000\n本页于合成日期核对。\n", "## A\n正文\n本页于合成日期核对。",
		"## A\n正文\n\n\t \r", "## A\n\xff\xe4\xb8\n", "# 说明\n## A\r\n正文\r\n## B\r\n",
		"## A\n\n本页于合成日期核对。\n## B\n正文",
	}
	random := rand.New(rand.NewSource(647))
	units := []string{"普通内容", "## A", "## B", "# 说明", "### 旧问题", "本页于", "\n", "\r\n", " ", "\t", "\u3000", "\xff", "\xe4\xb8", "\x00"}
	for range 1000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, faq := range texts {
		if got, want := qqFAQCategories(faq), previousQQFAQCategories(faq); !reflect.DeepEqual(got, want) {
			t.Fatalf("分类变化：输入=%q，结果=%q，期望=%q", faq, got, want)
		}
		for _, index := range []int{-1, 0, 1, 2, 5} {
			for _, footer := range []string{"本页于", "", "## ", " \t", "\n"} {
				got, ok := qqInsertFAQ(faq, index, "合成问题？\n继续", " \r\n合成答案\n第二行\t ", footer)
				want, found := previousQQInsertFAQ(faq, index, "合成问题？\n继续", " \r\n合成答案\n第二行\t ", footer)
				if ok != found || got != want {
					t.Fatalf("输入=%q，分类=%d，页尾=%q，结果=%q/%t，期望=%q/%t", faq, index, footer, got, ok, want, found)
				}
			}
		}
	}
}

func TestQQFAQStreamingAllocationBudget(t *testing.T) {
	faq := "# 合成 FAQ\n## 目标分类\n" + strings.Repeat("合成内容\n", 50000) + "\n本页于合成日期核对。\n"
	t.Run("分类", func(t *testing.T) {
		var got []string
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got = qqFAQCategories(faq)
			}
		})
		if !reflect.DeepEqual(got, []string{"目标分类"}) || result.AllocedBytesPerOp() > 4096 {
			t.Fatalf("分类=%q，分配=%s，期望不超过 4 KiB", got, result.MemString())
		}
	})
	for _, index := range []int{0, 2} {
		t.Run([]string{"插入", "无此分类"}[index/2], func(t *testing.T) {
			want, found := previousQQInsertFAQ(faq, index, "合成问题？", "合成答案", "本页于")
			var got string
			var ok bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, ok = qqInsertFAQ(faq, index, "合成问题？", "合成答案", "本页于")
				}
			})
			budget := int64(len(faq)) + (64 << 10)
			if !found {
				budget = 0
			}
			if got != want || ok != found || result.AllocedBytesPerOp() > budget {
				t.Fatalf("输出一致=%t，找到分类=%t/%t，分配=%s，预算=%d 字节", got == want, ok, found, result.MemString(), budget)
			}
		})
	}
}

func BenchmarkQQFAQStreaming(b *testing.B) {
	faq := "# 合成 FAQ\n## 目标分类\n" + strings.Repeat("合成内容\n", 50000) + "\n本页于合成日期核对。\n"
	for _, implementation := range []struct {
		name       string
		categories func(string) []string
		insert     func(string, int, string, string, string) (string, bool)
	}{
		{"previous", previousQQFAQCategories, previousQQInsertFAQ},
		{"current", qqFAQCategories, qqInsertFAQ},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			b.Run("分类", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.categories(faq)
				}
			})
			for _, index := range []int{0, 2} {
				b.Run([]string{"插入", "无此分类"}[index/2], func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_, _ = implementation.insert(faq, index, "合成问题？", "合成答案", "本页于")
					}
				})
			}
		})
	}
}

func TestQQInsertFAQCategoryPositions(t *testing.T) {
	body := strings.Repeat("普通行内 ## 非分类\r\n", 4096)
	for _, faq := range []string{
		body,
		strings.Repeat("单行内 ## 非分类 ", 4096),
		"## 首分类\n" + body + "## 末分类\r\n末分类正文  \r\n\u3000\n本页于合成日期核对。\n",
		body + "## 首分类\n正文\n## 末分类",
	} {
		for _, index := range []int{-1, 0, 1, 2, int(^uint(0) >> 1)} {
			got, ok := qqInsertFAQ(faq, index, "合成问题", "合成答案", "本页于")
			want, found := previousQQInsertFAQ(faq, index, "合成问题", "合成答案", "本页于")
			if got != want || ok != found {
				t.Fatalf("分类位置变化：分类=%d，结果长度=%d/%t，期望长度=%d/%t", index, len(got), ok, len(want), found)
			}
		}
	}
}

func BenchmarkQQInsertFAQCategoryPosition(b *testing.B) {
	body := strings.Repeat("合成内容\n", 50000)
	for _, tc := range []struct {
		name, faq string
		index     int
	}{
		{"首分类", "## 首分类\n" + body + "## 末分类\n尾部正文", 0},
		{"末分类", "## 首分类\n" + body + "## 末分类\n尾部正文", 1},
		{"无分类查第一个", body, 0},
		{"有分类查不存在的", "## 首分类\n" + body, 2},
		{"行内伪标题", strings.Repeat("正文 ## 非分类 ", 32768), 2},
		{"多分类查末尾", strings.Repeat("## 分类\n正文\n", 8192) + "## 末分类\n正文", 8192},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = qqInsertFAQ(tc.faq, tc.index, "合成问题", "合成答案", "本页于")
			}
		})
	}
}

func TestQQInsertFAQBodyBoundaries(t *testing.T) {
	for _, body := range []string{
		"", "\n", "正文", "正文  \t\r", "正文  \t\r\n\u3000\n \t\r\n",
		"\u0085\n\u00a0\n\u2000\n\u3000\n", "\xff\xe4\xb8 \r\n\u3000\n",
		"正文\n## 下一分类\n后续正文", "## 下一分类\n后续正文",
		"正文\n本页于核对\n## 下一分类", "本页于核对\n后续正文",
		"正文内本页于不算页尾\n正文内 ## 不算分类\n末行  \r\n\n",
		strings.Repeat("正文\n不同正文\n", 4096) + "末行  \r\n\u3000\n本页于核对\n",
		"末行  \r\n" + strings.Repeat(" \t\r\n\u3000\n", 4096) + "本页于核对\n",
	} {
		for _, footer := range []string{"本页于", "", "## ", "\n", "正文\n", "\r", "\x00", "\u3000", "末行  \r", strings.Repeat("长页尾", 64)} {
			faq := "# 合成 FAQ\r\n## 目标分类\r\n" + body
			got, ok := qqInsertFAQ(faq, 0, "合成问题", " \r\n合成答案\t ", footer)
			want, found := previousQQInsertFAQ(faq, 0, "合成问题", " \r\n合成答案\t ", footer)
			if got != want || ok != found {
				t.Fatalf("正文边界变化：正文=%q，页尾=%q，结果=%q/%t，期望=%q/%t", body, footer, got, ok, want, found)
			}
		}
	}
}

func TestQQInsertFAQWhitespaceLines(t *testing.T) {
	for _, padding := range []string{
		" \t\r\f\v", "\u0085\u00a0\u1680", "\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a",
		"\u2028\u2029\u202f\u205f\u3000", strings.Repeat(" \t\u3000", 32),
	} {
		for _, line := range []string{padding, padding + "正文", "正文" + padding, padding + "\xff", padding + "\u200b", padding + "\ufeff"} {
			faq := "## 目标分类\n正文\n" + line + "\n\n## 后续分类\n"
			got, ok := qqInsertFAQ(faq, 0, "合成问题", "合成答案", "本页于")
			want, found := previousQQInsertFAQ(faq, 0, "合成问题", "合成答案", "本页于")
			if got != want || ok != found {
				t.Fatalf("空白行判断变化：行=%q，结果=%q/%t，期望=%q/%t", line, got, ok, want, found)
			}
		}
	}
}

func BenchmarkQQInsertFAQBodyBoundary(b *testing.B) {
	body := strings.Repeat("合成内容\n不同正文\n", 25000)
	for _, tc := range []struct {
		name, body, footer string
	}{
		{"长正文到页尾", body + "\n本页于核对\n", "本页于"},
		{"长正文到分类", body + "\n## 下一分类\n正文", "本页于"},
		{"长正文到文末", body, "本页于"},
		{"紧邻页尾", "本页于核对\n" + body, "本页于"},
		{"短正文后页尾", "正文\n本页于核对\n" + body, "本页于"},
		{"紧邻分类", "## 下一分类\n" + body, "本页于"},
		{"长空白尾部", "正文  \r\n" + strings.Repeat(" \t\r\n\u3000\n", 50000), "本页于"},
		{"单行伪标记", strings.Repeat("正文 ## 非分类 本页于非页尾 ", 32768), "本页于"},
		{"含换行页尾", body, "正文\n"},
		{"短正文", "正文  \r\n\n本页于核对\n", "本页于"},
		{"长 ASCII 空白行", "正文\n" + strings.Repeat(" ", 400000), "本页于"},
		{"长 Unicode 空白行", "正文\n" + strings.Repeat("\u3000", 100000), "本页于"},
		{"超长末行", "正文\n" + strings.Repeat("合成正文", 40000) + "  \r\n", "本页于"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			faq := "## 目标分类\n" + tc.body
			b.ReportAllocs()
			for b.Loop() {
				_, _ = qqInsertFAQ(faq, 0, "合成问题", "合成答案", tc.footer)
			}
		})
	}
}
