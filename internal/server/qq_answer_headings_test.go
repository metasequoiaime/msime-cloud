package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestQQAnswerHeadingSpaceMatchesPreviousSet(t *testing.T) {
	for value := 0; value < 256; value++ {
		want := strings.ContainsRune(" \t\r\f", rune(value))
		if got := qqAnswerHeadingSpace(byte(value)); got != want {
			t.Fatalf("qqAnswerHeadingSpace(%d) = %v，预期 %v", value, got, want)
		}
	}
}

func TestQQStripAnswerHeadingsMatchesPreviousSemantics(t *testing.T) {
	pattern := regexp.MustCompile(`^#{1,6}\s*`)
	previous := func(text string) string {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			heading := pattern.FindString(line)
			if heading == "" {
				continue
			}
			if rest := line[len(heading):]; strings.TrimRight(heading, "#") == "" && rest != "" && rest[0] >= '0' && rest[0] <= '9' {
				continue
			}
			lines[i] = line[len(heading):]
		}
		return strings.Join(lines, "\n")
	}
	texts := []string{"", "#", "#\n", "#123\n## 标题\n#456", "## 标题\r\n正文\r\n##", "#\n##\n###", "普通\n\n## 标题\n\n"}
	for _, body := range []string{"普通正文\n", "正文中的 # 不算标题\n", "\n", "\xff\n"} {
		text := strings.Repeat(body, 1024)
		texts = append(texts, text+"## 末行标题", "#123\n"+text+"##\t\n#456\n####### 标题\n#")
	}
	for _, prefix := range []string{"", "普通正文"} {
		texts = append(texts, prefix+strings.Repeat("#", 65536)+"\n## 标题")
	}
	for hashes := 0; hashes <= 8; hashes++ {
		for _, space := range []string{"", " ", "\t", "\r", "\f", "\v", " \r\t\f", "\u0085", "\u3000", "\xff"} {
			for _, rest := range []string{"", "123", "标题", "#123", "\xff尾"} {
				texts = append(texts, strings.Repeat("#", hashes)+space+rest)
			}
		}
	}
	random := rand.New(rand.NewSource(7))
	units := []string{"#", " ", "\n", "\t", "\r", "\f", "\v", "\u3000", "\xff", "\xe4\xb8", "1", "a", "水", "😀"}
	for range 1000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		if got, want := qqStripAnswerHeadings(text), previous(text); got != want {
			t.Fatalf("标题清理不兼容：输入=%q，结果=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQNormalizeAnswerHeadingAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		allocations      float64
	}{
		{"编号无需改写", strings.Repeat("#123 已修复\n", 32768), strings.Repeat("#123 已修复\n", 32768), 0},
		{"标题逐行改写", strings.Repeat("## 操作说明\n", 32768), strings.Repeat("操作说明\n", 32768), 1},
		{"末行标题", strings.Repeat("普通回答\n", 32768) + "## 操作说明", strings.Repeat("普通回答\n", 32768) + "操作说明", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if allocations := testing.AllocsPerRun(3, func() { got = qqNormalizeAnswer(tc.text) }); allocations > tc.allocations {
				t.Fatalf("标题格式化分配 %.0f 次，超过 %.0f 次预算", allocations, tc.allocations)
			}
			if got != tc.want {
				t.Fatal("标题格式化结果发生变化")
			}
		})
	}
}

func TestQQStripAnswerHeadingsAllocatesForResult(t *testing.T) {
	text := "#\n##" + strings.Repeat(" ", 1<<20) + "\n#123"
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqStripAnswerHeadings(text)
		}
	})
	if got != "\n\n#123" {
		t.Fatalf("标题清理结果=%q，期望保留换行和编号", got)
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 4<<10 {
		t.Fatalf("短结果分配 %d 字节，超过 4 KiB 预算", allocated)
	}
}

func BenchmarkQQAnswerHeadings(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"short_issue", "#123 已修复\n#456 仍在处理"},
		{"short_heading", "## 操作说明\n第一步\n## 后续步骤\n第二步"},
		{"long_issues", strings.Repeat("#123 已修复\n", 32768)},
		{"long_headings", strings.Repeat("## 操作说明\n", 32768)},
		{"late_heading", strings.Repeat("普通回答\n", 32768) + "## 操作说明"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqNormalizeAnswer(tc.text)
			}
		})
	}
}

func BenchmarkQQStripAnswerHeadings(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"short", "## 操作说明\n第一步\n## 后续步骤\n第二步"},
		{"sparse", strings.Repeat("普通回答\n", 32768) + "## 操作说明"},
		{"inline_hashes", strings.Repeat("普通 # 回答\n", 32768) + "## 操作说明"},
		{"hash_run", strings.Repeat("#", 1<<18)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqStripAnswerHeadings(tc.text)
			}
		})
	}
}
