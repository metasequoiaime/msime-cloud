package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// 保留优化前的逐行拆分实现，用于对照重复行和空白处理。
func previousAddedLines(base, head string) int {
	remaining := map[string]int{}
	entries := func(content string, visit func(string)) {
		for _, line := range strings.Split(content, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				visit(line)
			}
		}
	}
	entries(base, func(line string) { remaining[line]++ })
	added := 0
	entries(head, func(line string) {
		if remaining[line] > 0 {
			remaining[line]--
		} else {
			added++
		}
	})
	return added
}

func TestAddedLinesValues(t *testing.T) {
	long := strings.Repeat("合成", 1<<18)
	for _, tc := range []struct {
		base string
		head string
		want int
	}{
		{"", "", 0},
		{"a\nb\n", "", 0},
		{"", "# 注释\n\na\na", 2},
		{"a\na\n", "a\na\na\n", 1},
		{"a\nb\n", "a\nc\nc\n", 2},
		{"a\na\nb\n", "b\na\na\n", 0},
		{"a\na\nb\n", "a\na\nb\n", 0},
		{" \u2003a\tA\r\n\t b \n# comment\n", "b\r\n\u2003a\tA \n # ignored\n\n", 0},
		{"\xff\n\x00\n", "\xff\n\x00\nnew", 1},
		{long + "\nlast", long + "\nlast\nnew", 1},
	} {
		if got := addedLines(tc.base, tc.head); got != tc.want {
			t.Fatalf("base字节数=%d head字节数=%d got=%d want=%d", len(tc.base), len(tc.head), got, tc.want)
		}
	}
}

func TestAddedLinesMatchesPrevious(t *testing.T) {
	random := rand.New(rand.NewSource(1229))
	values := []string{"", " ", "a", "b", "a\tA", "a\tB", " a\tA ", "\u2003a\tA\u2003", "# ignored", " \t# ignored", "\xff", "\x00"}
	content := func() string {
		var text strings.Builder
		for range random.Intn(40) {
			text.WriteString(values[random.Intn(len(values))])
			if random.Intn(3) == 0 {
				text.WriteByte('\r')
			}
			text.WriteByte('\n')
		}
		return strings.TrimSuffix(text.String(), []string{"", "\n"}[random.Intn(2)])
	}
	for sample := range 500 {
		base, head := content(), content()
		if got, want := addedLines(base, head), previousAddedLines(base, head); got != want {
			t.Fatalf("sample=%d got=%d want=%d", sample, got, want)
		}
	}
}

func TestAddedLinesRepeatedMemoryBudget(t *testing.T) {
	base := strings.Repeat("合成\tpinyin\t1\n", 5000)
	head := base + "合成\tpinyin\t1\nnew\tNew\t1\nnew\tNew\t1\n"
	var got int
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = addedLines(base, head)
		}
	})
	if got != 3 {
		t.Fatalf("重复行新增数 got=%d want=3", got)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 16<<10 {
		t.Fatalf("扫描五千行重复词条分配 %d 字节，超过 16 KiB 预算", bytes)
	}
}

func TestAddedLinesUnchangedAllocationBudget(t *testing.T) {
	var text strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&text, "synthetic%04d\tDisplay%04d\t1\n", i, i)
	}
	base := text.String()
	for _, tc := range []struct {
		name string
		head string
	}{
		{"相同字符串", base},
		{"相同内容独立副本", strings.Clone(base)},
		{"全部删除", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			allocations := testing.AllocsPerRun(3, func() { got = addedLines(base, tc.head) })
			if got != 0 {
				t.Fatalf("无新增词条时 got=%d want=0", got)
			}
			if allocations > 4 {
				t.Fatalf("无新增词条仍分配 %.0f 次，超过 4 次预算", allocations)
			}
		})
	}
}

func BenchmarkAddedLines(b *testing.B) {
	var distinct strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&distinct, "synthetic%04d\tDisplay%04d\t1\n", i, i)
	}
	for _, tc := range []struct {
		name string
		base string
		head string
		want int
	}{
		{"distinct", distinct.String(), distinct.String() + "new\tNew\t1\n", 1},
		{"repeated", strings.Repeat("合成\tpinyin\t1\n", 5000), strings.Repeat("合成\tpinyin\t1\n", 5003), 3},
		{"empty", "", "", 0},
		{"unchanged", distinct.String(), distinct.String(), 0},
		{"unchanged_copy", distinct.String(), strings.Clone(distinct.String()), 0},
		{"deleted", distinct.String(), "", 0},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string, string) int
		}{{"previous", previousAddedLines}, {"current", addedLines}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := impl.fn(tc.base, tc.head); got != tc.want {
						b.Fatalf("got=%d want=%d", got, tc.want)
					}
				}
			})
		}
	}
}
