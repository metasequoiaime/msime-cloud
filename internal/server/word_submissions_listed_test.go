package server

import (
	"bufio"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func previousListedLines(content string, lines []submissionLine) alreadyListedError {
	existing := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Split(line, "\t"); len(fields) >= 2 {
			existing[strings.TrimSpace(fields[0])+"\t"+strings.TrimSpace(fields[1])] = true
		}
	}
	var listed alreadyListedError
	for i, line := range lines {
		if existing[line.key] {
			listed = append(listed, i)
		}
	}
	return listed
}

func listedLinesFixture(rows int) string {
	var text strings.Builder
	for i := range rows {
		fmt.Fprintf(&text, "synthetic%04d\tDisplay%04d\t1\n", i, i)
	}
	return text.String()
}

func TestListedLinesMatchesPrevious(t *testing.T) {
	oversized := strings.Repeat("x", 1<<20)
	cases := []struct {
		content string
		keys    []string
	}{
		{"", nil}, {"", []string{"a\tb"}},
		{"# 说明\n\n a \t b \t3\textra\r\n", []string{"a\tb", "missing\tMissing", "a\tb"}},
		{"a\t\t3\nonly\n\tword\t1\n", []string{"a\t", "word\t1", "only"}},
		{"b\tB\t1\na\tA\t2\na\tA\t3\n", []string{"a\tA", "b\tB", "a\tA"}},
		{"\u2003a \t b\u2003\t1\r\n", []string{"a\tb"}},
		{"a\tb\t" + strings.Repeat("ignored\t", 1000) + "\r\n", []string{"a\tb", "a\tb\tignored"}},
		{"a\t" + strings.Repeat("合成", 64) + "\t1\nb\tB\t2\n", []string{"a\t" + strings.Repeat("合成", 64), "b\tB"}},
		{oversized + "\na\tA\t1\n", []string{"a\tA"}},
		{"a\tA\t1\n" + oversized + "\nb\tB\t1", []string{"a\tA", "b\tB"}},
		{"a\tA\t1\n" + oversized, []string{"a\tA", "a\tA"}},
	}
	random := rand.New(rand.NewSource(719))
	fields := []string{"a", "b", "A", "B", "", "#ignored", "3", " x ", "\xff", "\u2003a"}
	for range 500 {
		var text strings.Builder
		for range random.Intn(30) {
			for i := range random.Intn(5) {
				if i > 0 {
					text.WriteByte('\t')
				}
				text.WriteString(fields[random.Intn(len(fields))])
			}
			if random.Intn(3) == 0 {
				text.WriteByte('\r')
			}
			text.WriteByte('\n')
		}
		keys := []string{"a\tA", "b\tB", "a\t", "x\t3", "missing\tMissing"}
		cases = append(cases, struct {
			content string
			keys    []string
		}{text.String(), keys[:random.Intn(len(keys)+1)]})
	}
	for i, tc := range cases {
		lines := make([]submissionLine, len(tc.keys))
		for j, key := range tc.keys {
			lines[j] = submissionLine{key: key}
		}
		got, want := listedLines(tc.content, lines), previousListedLines(tc.content, lines)
		if !slices.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("case=%d got=%v want=%v", i, got, want)
		}
	}
}

func TestListedLinesMemoryBudget(t *testing.T) {
	content := listedLinesFixture(5000)
	lines := []submissionLine{{key: "missing\tMissing"}}
	var got alreadyListedError
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = listedLines(content, lines)
		}
	})
	if got != nil {
		t.Fatalf("不存在的词条被标为已有：%v", got)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 240<<10 {
		t.Fatalf("扫描五千行词库分配 %d 字节，超过 240 KiB 预算", bytes)
	}
}

func TestListedLinesEarlyMatchAllocationBudget(t *testing.T) {
	content := listedLinesFixture(5000)
	lines := []submissionLine{{key: "synthetic0000\tDisplay0000"}, {key: "synthetic0000\tDisplay0000"}}
	var got alreadyListedError
	allocations := testing.AllocsPerRun(3, func() { got = listedLines(content, lines) })
	if !slices.Equal(got, alreadyListedError{0, 1}) {
		t.Fatalf("重复输入的索引发生变化：%v", got)
	}
	if allocations > 20 {
		t.Fatalf("首行已命中全部待查键，仍分配 %.0f 次，超过 20 次预算", allocations)
	}
}

func TestListedLinesLongFieldsMemoryBudget(t *testing.T) {
	var text strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&text, "synthetic%04d\t%s%d\t1\n", i, strings.Repeat("合成", 20), i)
	}
	content := text.String()
	lines := []submissionLine{{key: "missing\tMissing"}}
	var got alreadyListedError
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = listedLines(content, lines)
		}
	})
	if got != nil {
		t.Fatalf("不存在的词条被标为已有：%v", got)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 1<<20 {
		t.Fatalf("扫描五千行长词条分配 %d 字节，超过 1 MiB 预算", bytes)
	}
}

func BenchmarkListedLines(b *testing.B) {
	for _, rows := range []int{100, 5000} {
		content := listedLinesFixture(rows)
		for _, tc := range []struct {
			name  string
			lines []submissionLine
			want  alreadyListedError
		}{
			{"first", []submissionLine{{key: "synthetic0000\tDisplay0000"}}, alreadyListedError{0}},
			{"last", []submissionLine{{key: fmt.Sprintf("synthetic%04d\tDisplay%04d", rows-1, rows-1)}}, alreadyListedError{0}},
			{"missing", []submissionLine{{key: "missing\tMissing"}}, nil},
			{"empty", nil, nil},
		} {
			b.Run(fmt.Sprintf("%d/%s", rows, tc.name), func(b *testing.B) {
				for _, impl := range []struct {
					name string
					fn   func(string, []submissionLine) alreadyListedError
				}{{"previous", previousListedLines}, {"current", listedLines}} {
					b.Run(impl.name, func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							if got := impl.fn(content, tc.lines); !slices.Equal(got, tc.want) {
								b.Fatal(got)
							}
						}
					})
				}
			})
		}
	}
}
