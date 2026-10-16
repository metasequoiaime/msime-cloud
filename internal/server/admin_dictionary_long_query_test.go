package server

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestDictionaryLongLineQueryCompatibility(t *testing.T) {
	lines := []string{
		strings.Repeat("a", 5000), strings.Repeat("A", 5000), strings.Repeat("a", 5000) + "WORDB",
		strings.Repeat("水杉İΣȺ𐐀", 500), strings.Repeat("\xff\xfe", 2000),
		strings.Repeat("\ufffd", 1000),
	}
	for _, boundary := range []int{255, 256, 257, 1020, 1023, 1024, 1025, 2044, 2047, 2048, 2049} {
		lines = append(lines, strings.Repeat("A", boundary)+"iΣȺ𐐀\xff\ufffd末尾"+strings.Repeat("B", 2500))
	}
	rng := rand.New(rand.NewSource(680))
	atoms := []string{"a", "A", "水杉", "İ", "Σ", "Ⱥ", "𐐀", "\xff", "\xe4\xb8", "\ufffd"}
	for range 500 {
		var line strings.Builder
		for range rng.Intn(2500) + 256 {
			line.WriteString(atoms[rng.Intn(len(atoms))])
		}
		lines = append(lines, line.String())
	}
	for i, line := range lines {
		lower := strings.ToLower(line)
		queries := []string{"", "a", "i", "σ", "aa", "ab", "aaword", "wordb", "missing", "iσⱥ𐐨", "𐐨\ufffd\ufffd末", "水杉", "\ufffd", strings.Repeat("a", 199) + "b", strings.Repeat("a", 1024), strings.Repeat("a", 1024) + "b", strings.Repeat("水", 1500)}
		for range 5 {
			start := rng.Intn(len(lower) + 1)
			end := min(len(lower), start+rng.Intn(1100))
			queries = append(queries, lower[start:end])
		}
		for _, query := range queries {
			query = strings.ToLower(query)
			if got, want := dictionaryLineContainsLower(line, query), strings.Contains(lower, query); got != want {
				t.Fatalf("第 %d 行查询=%q，匹配=%t，期望=%t", i, query, got, want)
			}
		}
	}
}

func TestDictionaryRowsLongQueryMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ name, line, query string }{
		{"长英文译文命中", "Waterfir\t" + strings.Repeat("SYNTHETIC TRANSLATION ", 80) + "\t100\n", "translation"},
		{"长Unicode未命中", strings.Repeat("水杉İΣ ", 200) + "\t100\n", "missing"},
		{"跨缓冲区命中", strings.Repeat("A", 2044) + "İΣȺ𐐀\t100\n", "iσⱥ𐐨"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat(tc.line, 5000)
			var rows []dictionaryRow
			var total int
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					rows, total = dictionaryRows("custom/translations.txt", text, tc.query, 2)
				}
			})
			want, expected := previousDictionaryRows("custom/translations.txt", text, tc.query, 2)
			if total != expected || !reflect.DeepEqual(rows, want) {
				t.Fatal("长行查询结果、总数或分页发生变化")
			}
			t.Logf("%s; %s", result, result.MemString())
			// 保留当前页列切片的预算，禁止为每行复制完整小写文本。
			if allocated := result.AllocedBytesPerOp(); allocated > 64<<10 {
				t.Fatalf("五千行长词条查询分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func TestDictionaryLongLineQueryAllocations(t *testing.T) {
	for _, line := range []string{strings.Repeat("A", 10000), strings.Repeat("Ⱥ", 10000), strings.Repeat("\xff", 10000)} {
		var got bool
		if allocated := testing.AllocsPerRun(20, func() { got = dictionaryLineContainsLower(line, "missing") }); allocated != 0 {
			t.Fatalf("%d 字节长行查询分配 %g 次，期望零分配", len(line), allocated)
		}
		if got {
			t.Fatal("未命中查询返回匹配")
		}
	}
}

func TestDictionaryQueryMaximumLengthAllocations(t *testing.T) {
	query := strings.Repeat("a", 199) + "b"
	for _, line := range []string{"WORD", "水杉İΣ", strings.Repeat("A", 256), strings.Repeat("A", 199) + "B", strings.Repeat("Ⱥ", 100), strings.Repeat("\xff", 256), strings.Repeat("A", 10000), strings.Repeat("水杉İΣ", 1000)} {
		var got bool
		if allocated := testing.AllocsPerRun(20, func() { got = dictionaryLineContainsLower(line, query) }); allocated != 0 {
			t.Fatalf("%d 字节行匹配 200 字节查询时分配 %g 次，期望零分配", len(line), allocated)
		}
		if want := strings.Contains(strings.ToLower(line), query); got != want {
			t.Fatalf("匹配=%t，期望=%t", got, want)
		}
	}
}

func BenchmarkDictionaryLongLineQuery(b *testing.B) {
	for _, tc := range []struct{ name, line, query string }{
		{"ShortASCIIQuery", strings.Repeat("A", 256), strings.Repeat("a", 199) + "b"},
		{"ShortASCIIMatch", strings.Repeat("A", 199) + "B", strings.Repeat("a", 199) + "b"},
		{"ASCIIMatch", "WORD " + strings.Repeat("SYNTHETIC TRANSLATION ", 80), "word"},
		{"ASCIIMiss", strings.Repeat("SYNTHETIC TRANSLATION ", 80), "missing"},
		{"LowercaseMiss", strings.Repeat("synthetic translation ", 80), "missing"},
		{"ChineseMiss", strings.Repeat("水杉输入法", 200), "missing"},
		{"MixedMiss", strings.Repeat("水杉İΣ", 200), "missing"},
		{"Invalid", strings.Repeat("\xff", 2000), "missing"},
		{"RepeatedPrefix", strings.Repeat("AA", 5000), strings.Repeat("a", 199) + "b"},
	} {
		for _, impl := range []struct {
			name  string
			match func(string, string) bool
		}{
			{"Previous", previousDictionaryLineContainsLower}, {"Current", dictionaryLineContainsLower},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					dictionaryUnicodeQueryResult = impl.match(tc.line, tc.query)
				}
			})
		}
	}
}
