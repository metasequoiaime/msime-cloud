package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestDictionaryRowsFormatColumnSemantics(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"source.csv", " \t词\t语,100,注音 \t\r\n"},
		{"source.csv", "\t词\t未\"闭,100\r\n"},
		{"source.csv", "词,100,\"未闭\t列\r\n"},
		{"sources/korean/hanja.csv", "词\t语,100,注音\n"},
		{"sources/korean/hanja.txt", "  한\t자:漢\t字:10:余 \r\n"},
		{"sources/korean/hanja.txt", "  한\t자  \r\n"},
		{"source.md", "  **词 语**  \r\n"},
		{"source.txt", "  词 语 100 \r\n"},
	} {
		got, total := dictionaryRows(tc.name, tc.text, "", 1)
		want, expected := previousDictionaryRows(tc.name, tc.text, "", 1)
		if total != expected || !reflect.DeepEqual(got, want) {
			t.Fatalf("文件=%q，输入=%q，结果=%+v/%d，期望=%+v/%d", tc.name, tc.text, got, total, want, expected)
		}
	}
}

func TestDictionaryRowsFormatColumnAllocationBudget(t *testing.T) {
	field := "词" + strings.Repeat("\t", 4096) + "语"
	for _, tc := range []struct {
		name, line string
		columns    []string
		budget     int64
	}{
		{"source.csv", field + ",100,注音\n", []string{field, "100", "注音"}, 4 << 20},
		{"sources/korean/hanja.txt", field + ":漢字:100\n", []string{field, "漢字", "100"}, 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat(tc.line, 100)
			var rows []dictionaryRow
			var total int
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					rows, total = dictionaryRows(tc.name, text, "", 1)
				}
			})
			if total != 100 || len(rows) != 100 || rows[0].Line != 1 || rows[99].Line != 100 || !reflect.DeepEqual(rows[0].Columns, tc.columns) || !reflect.DeepEqual(rows[99].Columns, tc.columns) {
				t.Fatal("预览的格式解析、行号或总数发生变化")
			}
			if result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("预览分配 %s，超过 %d 字节预算", result.MemString(), tc.budget)
			}
		})
	}
}

func BenchmarkDictionaryRowsFormatColumns(b *testing.B) {
	field := "词" + strings.Repeat("\t", 4096) + "语"
	for _, tc := range []struct{ name, line string }{
		{"source.csv", field + ",100,注音\n"},
		{"sources/korean/hanja.txt", field + ":漢字:100\n"},
	} {
		text := strings.Repeat(tc.line, 100)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = dictionaryRows(tc.name, text, "", 1)
			}
		})
	}
}
