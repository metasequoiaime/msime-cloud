package server

import (
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// qqMarkdownNoise 保留旧清理正则，供兼容性测试对照。
var qqMarkdownNoise = regexp.MustCompile("\\]\\([^)]*\\)|[`*_>#\\[|]")

func TestQQListLabelsKeepsBoundariesAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"列表符号和换行", "  - 换肤：说明\r\n\t* 删除: 说明\n普通：正文", []string{"换肤", "删除"}},
		{"最后允许的位置", "- " + strings.Repeat("水", 11) + "：说明", []string{strings.Repeat("水", 11)}},
		{"超过位置上限", "- " + strings.Repeat("水", 12) + ":说明", nil},
		{"边界后的链接斜杠", "- " + strings.Repeat("水", 11) + ":/说明", nil},
		{"全角斜杠不是链接", "- " + strings.Repeat("水", 11) + ":／说明", []string{strings.Repeat("水", 11)}},
		{"四字节字符", "- " + strings.Repeat("😀", 11) + ":说明", []string{strings.Repeat("😀", 11)}},
		{"空标签和链接", "- :说明\n- **：说明\n- https://example.test\n* 没有冒号", nil},
		{"清理符号并保留首次顺序", "- **换肤**：说明\n* [删除](x):说明\n- 换肤：重复", []string{"换肤", "删除"}},
		{"无效编码", "- 水\xff\xfe：说明", []string{"水��"}},
		{"空文本", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqListLabels(tc.text); !slices.Equal(got, tc.want) {
				t.Fatalf("列表标签=%q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestQQListLabelsMatchesPreviousSemantics(t *testing.T) {
	previous := func(text string) []string {
		var labels []string
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			rest, ok := strings.CutPrefix(line, "- ")
			if !ok {
				if rest, ok = strings.CutPrefix(line, "* "); !ok {
					continue
				}
			}
			runes := []rune(rest)
			i := slices.IndexFunc(runes, func(r rune) bool { return r == '：' || r == ':' })
			if i < 0 || i >= qqListLabelRunes || (i+1 < len(runes) && runes[i+1] == '/') {
				continue
			}
			label := strings.Join(strings.Fields(qqMarkdownNoise.ReplaceAllString(string(runes[:i]), " ")), " ")
			if label != "" && !slices.Contains(labels, label) {
				labels = append(labels, label)
			}
		}
		return labels
	}
	random := rand.New(rand.NewSource(3))
	for size := 0; size <= 64; size++ {
		data := make([]byte, size)
		if _, err := random.Read(data); err != nil {
			t.Fatal(err)
		}
		for prefix := 0; prefix <= qqListLabelRunes+1; prefix++ {
			for _, colon := range []string{":", "：", ":/", "：/"} {
				text := "- " + strings.Repeat("水", prefix) + string(data) + colon + "说明\n* " + string(data) + colon + "尾部"
				if got, want := qqListLabels(text), previous(text); !slices.Equal(got, want) {
					t.Fatalf("列表标签不兼容：text=%q，结果=%q，期望=%q", text, got, want)
				}
			}
		}
	}
	texts := []string{
		"- [甲](x)：说明", "- ](x)[甲：说明", "- ]([](x)尾：说明", "- ](未闭合[甲：说明",
		"- `甲`\u3000乙：说明", "- 甲\xff：说明\n- 甲�：重复", "- 甲|乙：说明\n- 甲 乙：重复",
	}
	units := []string{"水", "😀", "a", " ", "\t", "\u3000", "\xff", "\xe4\xb8", "`", "*", ">", "#", "_", "[", "](", ")", "|", ":", "：", "/"}
	for range 500 {
		var input strings.Builder
		input.WriteString("- ")
		for range random.Intn(15) {
			input.WriteString(units[random.Intn(len(units))])
		}
		input.WriteString("：说明")
		texts = append(texts, input.String())
	}
	for _, text := range texts {
		if got, want := qqListLabels(text), previous(text); !slices.Equal(got, want) {
			t.Fatalf("列表标签清理不兼容：text=%q，结果=%q，期望=%q", text, got, want)
		}
	}
}

func TestQQListLabelsReusesNormalizationMemory(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"重复标签", strings.Repeat("- 换肤：说明\n", 32768), []string{"换肤"}},
		{"不同标记清理后重复", strings.Repeat("- 换肤：说明\n- `换肤`：说明\n- [换肤](x):说明\n- _换肤_：说明\n", 8192), []string{"换肤"}},
		{"无效编码清理后重复", strings.Repeat("- 水\xff：说明\n- 水�：说明\n", 16384), []string{"水�"}},
		{"最大四字节标签", strings.Repeat("- "+strings.Repeat("😀", 11)+"：说明\n", 32768), []string{strings.Repeat("😀", 11)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			if allocations := testing.AllocsPerRun(3, func() { got = qqListLabels(tc.text) }); allocations > 8 {
				t.Fatalf("重复标签清理分配 %.0f 次，超过 8 次预算", allocations)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("重复标签=%q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestQQListLabelsAvoidsAllocatingForUnmatchedText(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"长列表行", "- " + strings.Repeat("水", 65536)},
		{"很多普通行", strings.Repeat("普通正文\n", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			if allocations := testing.AllocsPerRun(10, func() { got = qqListLabels(tc.text) }); allocations != 0 {
				t.Fatalf("没有标签时分配 %.0f 次，期望不为全文或行切片分配内存", allocations)
			}
			if got != nil {
				t.Fatalf("不应提取到标签：%q", got)
			}
		})
	}
}

func BenchmarkQQListLabels(b *testing.B) {
	long := strings.Repeat("水", 65536)
	for _, tc := range []struct {
		name, text string
		labels     int
	}{
		{"long_description", "- 换肤：" + long, 1},
		{"no_colon", "- " + long, 0},
		{"late_colon", "- " + long + "：说明", 0},
		{"ordinary_lines", strings.Repeat("普通正文\n", 8192), 0},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var got []string
			for b.Loop() {
				got = qqListLabels(tc.text)
			}
			if len(got) != tc.labels {
				b.Fatalf("标签数量=%d，期望 %d", len(got), tc.labels)
			}
		})
	}
}
