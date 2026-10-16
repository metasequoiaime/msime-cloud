package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestDictionaryRowsQueryMatchesPrevious(t *testing.T) {
	texts := []string{
		"SYNTHETIC\tWORD\t100\n", "synthetic\tword\t100\n", "水杉İΣſKẞ\tABC\t100\n",
		"\xffWORD\xe4\xb8\t100\n", "\uFFFDWORD\uFFFD\t100\n", "# WORD\n; WORD\n// WORD\n",
	}
	for _, size := range []int{255, 256, 257, 1024} {
		texts = append(texts, strings.Repeat("A", size-4)+"WORD\n", strings.Repeat("A", size-4)+"KX\n")
	}
	for _, text := range texts {
		for _, query := range []string{"", "WORD", "word", "missing", "k", "ſ", "S", "İ", "i", "σ", "ς", "ß", "SS", "\xff", "\uFFFD"} {
			for _, page := range []int{0, 1, 2} {
				got, total := dictionaryRows("custom/english.txt", text, query, page)
				want, expected := previousDictionaryRows("custom/english.txt", text, query, page)
				if total != expected || !reflect.DeepEqual(got, want) {
					t.Fatalf("输入=%q，查询=%q，页码=%d，结果=%+v/%d，期望=%+v/%d", text, query, page, got, total, want, expected)
				}
			}
		}
	}
}

func TestDictionaryRowsASCIIQueryAllocationBudget(t *testing.T) {
	text := strings.Repeat("SYNTHETIC\tWORD\t100\n", 50000)
	for _, query := range []string{"WORD", "missing"} {
		t.Run(query, func(t *testing.T) {
			var rows []dictionaryRow
			var total int
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					rows, total = dictionaryRows("custom/english.txt", text, query, 2)
				}
			})
			if query == "WORD" {
				if total != 50000 || len(rows) != 100 || rows[0].Line != 101 || rows[99].Line != 200 {
					t.Fatal("查询结果的行号、分页或总数发生变化")
				}
			} else if total != 0 || len(rows) != 0 {
				t.Fatal("无匹配查询返回了词条")
			}
			if result.AllocedBytesPerOp() > 64<<10 {
				t.Fatalf("五万行查询分配 %s，超过 64 KiB 预算", result.MemString())
			}
		})
	}
}

func BenchmarkDictionaryRowsQuery(b *testing.B) {
	for _, tc := range []struct{ name, line, query string }{
		{"uppercase_match", "SYNTHETIC\tWORD\t100\n", "WORD"},
		{"uppercase_miss", "SYNTHETIC\tWORD\t100\n", "missing"},
		{"lowercase_match", "synthetic\tword\t100\n", "WORD"},
		{"unicode_match", "水杉İΣ\tWORD\t100\n", "WORD"},
	} {
		text := strings.Repeat(tc.line, 50000)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = dictionaryRows("custom/english.txt", text, tc.query, 1)
			}
		})
	}
}
