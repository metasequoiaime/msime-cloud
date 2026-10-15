package server

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
)

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
