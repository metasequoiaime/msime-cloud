package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestDictionaryRowsWhitespaceColumns(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		columns    []string
	}{
		{"source.txt", "  synthetic word 100 \r\n", []string{"synthetic", "word", "100"}},
		{"source.txt", "  水杉\u3000shui'shan\u00a0100 \r\n", []string{"水杉", "shui'shan", "100"}},
		{"source.txt", "\xff\vword\f100\r\n", []string{"\xff", "word", "100"}},
		{"source.txt", "\t水杉 word 100 \r\n", []string{"", "水杉 word 100 "}},
		{"source.txt", " 水杉 word 100\t\r\n", []string{" 水杉 word 100", ""}},
		{"source.txt", " 水杉\t\tword\t100 \r\n", []string{" 水杉", "", "word", "100 "}},
		{"source.md", "  **水杉 word**  \r\n", []string{"  **水杉 word**  "}},
		{"source.csv", "  水杉 word,100 \r\n", []string{"水杉 word", "100"}},
		{"sources/korean/hanja.txt", "  한 자:漢 字:100 \r\n", []string{"한 자", "漢 字", "100"}},
	} {
		t.Run(tc.name+"/"+tc.text, func(t *testing.T) {
			rows, total := dictionaryRows(tc.name, "# 注释\r\n"+tc.text, "", 1)
			if total != 1 || len(rows) != 1 || rows[0].Line != 2 || !reflect.DeepEqual(rows[0].Columns, tc.columns) {
				t.Fatalf("预览分列发生变化：结果=%+v/%d，期望列=%q", rows, total, tc.columns)
			}
		})
	}
}

func TestDictionaryRowsWhitespaceColumnAllocationBudget(t *testing.T) {
	for _, line := range []string{"synthetic word 100\n", "水杉\u3000shui'shan\u00a0100\n"} {
		t.Run(line, func(t *testing.T) {
			text := strings.Repeat(line, 200)
			var rows []dictionaryRow
			var total int
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					rows, total = dictionaryRows("source.txt", text, "", 2)
				}
			})
			if total != 200 || len(rows) != 100 || rows[0].Line != 101 || rows[99].Line != 200 || len(rows[0].Columns) != 3 {
				t.Fatal("空白分列词库的分页、列数或总数发生变化")
			}
			if allocations := result.AllocsPerOp(); allocations > 120 {
				t.Fatalf("每页分配 %d 次，超过 120 次预算", allocations)
			}
		})
	}
}

func BenchmarkDictionaryRowsWhitespaceColumns(b *testing.B) {
	for _, tc := range []struct{ name, file, line string }{
		{"ascii", "source.txt", "synthetic word 100\n"},
		{"unicode", "source.txt", "水杉\u3000shui'shan\u00a0100\n"},
		{"tabs", "source.txt", "水杉\tshui'shan\t100\n"},
		{"markdown", "source.md", "  **水杉 word**  \r\n"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			text := strings.Repeat(tc.line, 100)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, _ = dictionaryRows(tc.file, text, "", 1)
			}
		})
	}
}
