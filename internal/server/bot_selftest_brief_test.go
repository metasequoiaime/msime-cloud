package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// previousQQSelfTestBrief 保留先拆分全部行再检查字数的旧实现，核对错误文案和分配开销。
func previousQQSelfTestBrief(answer string, runes, lines int) string {
	count := 0
	for _, line := range strings.Split(answer, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	switch length := utf8.RuneCountInString(answer); {
	case length > runes:
		return fmt.Sprintf("回答 %d 字，超过 %d 字", length, runes)
	case count > lines:
		return fmt.Sprintf("回答 %d 行，超过 %d 行", count, lines)
	}
	return ""
}

func TestQQSelfTestBriefCompatibility(t *testing.T) {
	texts := []string{
		"", "正文", "正文\n", "\n正文\n\n尾部\n", " \t\r\n\u0085\u00a0\u1680\u2000\u2009\u2028\u2029\u202f\u205f\u3000",
		"正文\r\n尾部\r\n", "\xff\n\xe2\x80\n\x00\n", "\u2028正文\u2029尾部", strings.Repeat("字", 201),
	}
	random := rand.New(rand.NewSource(1067))
	units := []string{"正文", "a", "😀", " ", "\t", "\r", "\n", "\u0085", "\u3000", "\xff", "\xe2\x80", "\x00"}
	for range 200 {
		var answer strings.Builder
		for range random.Intn(80) {
			answer.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, answer.String())
	}
	for _, answer := range texts {
		for _, runes := range []int{-1, 0, 1, 200, utf8.RuneCountInString(answer)} {
			for _, lines := range []int{-1, 0, 1, 6, 200} {
				if got, want := qqSelfTestBrief(answer, runes, lines), previousQQSelfTestBrief(answer, runes, lines); got != want {
					t.Fatalf("自测限制结果变化：输入=%q，字数上限=%d，行数上限=%d，结果=%q，期望=%q", answer, runes, lines, got, want)
				}
			}
		}
	}
}

func TestQQSelfTestBriefReportsExactCounts(t *testing.T) {
	for _, sample := range []struct {
		answer       string
		runes, lines int
		want         string
	}{
		{strings.Repeat("正文\n", 100), 200, 6, "回答 300 字，超过 200 字"},
		{"\n正文\n \t\u3000\n尾部\n", 200, 1, "回答 2 行，超过 1 行"},
	} {
		if got := qqSelfTestBrief(sample.answer, sample.runes, sample.lines); got != sample.want {
			t.Fatalf("自测限制的优先级或实际计数变化：结果=%q，期望=%q", got, sample.want)
		}
	}
}

func TestQQSelfTestBriefAllocationBudget(t *testing.T) {
	for _, sample := range []struct {
		name, answer string
		runes, lines int
		budget       int64
	}{
		{"short", "Windows：卸载应用\nmacOS：移到废纸篓", 200, 6, 0},
		{"many_lines_valid", strings.Repeat("正文\n", 32768), 1 << 30, 1 << 30, 0},
		{"overlong", strings.Repeat("正文\n", 32768), 200, 6, 4 << 10},
	} {
		t.Run(sample.name, func(t *testing.T) {
			want := previousQQSelfTestBrief(sample.answer, sample.runes, sample.lines)
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqSelfTestBrief(sample.answer, sample.runes, sample.lines)
				}
			})
			if got != want || result.AllocedBytesPerOp() > sample.budget {
				t.Fatalf("自测限制结果变化或分配超预算：结果=%q，期望=%q，分配=%s，预算=%d B/op", got, want, result.MemString(), sample.budget)
			}
		})
	}
}

func BenchmarkQQSelfTestBrief(b *testing.B) {
	for _, sample := range []struct {
		name, answer string
		runes, lines int
	}{
		{"single_line", "你用的是哪个系统？", 200, 6},
		{"short", "Windows：卸载应用\nmacOS：移到废纸篓", 200, 6},
		{"many_lines_valid", strings.Repeat("正文\n", 32768), 1 << 30, 1 << 30},
		{"overlong", strings.Repeat("正文\n", 32768), 200, 6},
		{"too_many_lines", strings.Repeat("一行\n", 7), 200, 6},
	} {
		for _, impl := range []struct {
			name string
			run  func(string, int, int) string
		}{{"previous", previousQQSelfTestBrief}, {"current", qqSelfTestBrief}} {
			b.Run(sample.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(sample.answer, sample.runes, sample.lines)
				}
			})
		}
	}
}

// 没通过的题的日志要分清「没去查」和「查了网上还是认不出」；小标题里本身就有「据网上资料」，不能据此判断。
func TestQQWebSourced(t *testing.T) {
	for material, want := range map[string]bool{
		"": false,
		"## Windows 正式版 · 输入方案\n在设置里选五笔":                               false,
		"文档\n" + qqResearchHeading + "GitHub 上有 12 个贡献者\n":             false,
		"文档\n" + qqResearchHeading + "据网上资料，Photon Studio 是图像编辑软件\n":   true,
		"据网上资料，这是文档里的说法\n" + qqResearchHeading + "GitHub 上有 12 个贡献者\n": false,
	} {
		if got := qqWebSourced(material); got != want {
			t.Errorf("%q: %v want %v", material, got, want)
		}
	}
}
