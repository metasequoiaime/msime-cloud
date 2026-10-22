package server

import (
	"encoding/csv"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func previousDictionaryRows(name, text, query string, page int) ([]dictionaryRow, int) {
	rows := []dictionaryRow{}
	total := 0
	q := strings.ToLower(query)
	yamlHeader := strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if yamlHeader {
			if line == "..." {
				yamlHeader = false
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") || !strings.Contains(strings.ToLower(line), q) {
			continue
		}
		total++
		if total <= (page-1)*100 || total > page*100 {
			continue
		}
		cols := strings.Split(strings.TrimSuffix(raw, "\r"), "\t")
		if strings.HasSuffix(name, ".csv") {
			reader := csv.NewReader(strings.NewReader(line))
			reader.FieldsPerRecord = -1
			if parsed, err := reader.Read(); err == nil {
				cols = parsed
			}
		} else if strings.HasPrefix(name, "sources/korean/") {
			cols = strings.SplitN(line, ":", 3)
		} else if len(cols) == 1 && !strings.HasSuffix(name, ".md") {
			cols = strings.Fields(line)
		}
		rows = append(rows, dictionaryRow{Line: i + 1, Columns: cols})
	}
	return rows, total
}

func TestDictionaryRowsLineScanMatchesPrevious(t *testing.T) {
	texts := []string{
		"", "\n", "水杉\tshui'shan\t1", "\r\n水杉\tshui'shan\t1\r\n",
		"# 注释\n; 注释\n// 注释\n  水杉  100 \n\n",
		"---\nname: ignored\n...\n词语\tci'yu\t1\n",
		"---\n没有结束标记\n词语\tci'yu\t1\n",
		"\"词,语\",100,\"注音\"\n\"没有结束的引号\n",
		"漢字:한자:100\n  **水杉 词语**  \r\n\xff\xe4\xb8\n",
	}
	random := rand.New(rand.NewSource(83))
	units := []string{"水杉", "word", "WORD", "\t", " ", "\r", "\n", "#", ";", "//", "...", "---", ":", ",", "\"", "\xff", "\xe4\xb8"}
	for range 100 {
		var text strings.Builder
		for range random.Intn(200) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String())
	}
	for _, name := range []string{"custom/words.txt", "source.csv", "source.yaml", "source.yml", "sources/korean/hanja.txt", "source.md"} {
		for _, text := range texts {
			for _, query := range []string{"", "水杉", "WORD"} {
				for _, page := range []int{0, 1, 2} {
					got, total := dictionaryRows(name, text, query, page)
					want, expected := previousDictionaryRows(name, text, query, page)
					if total != expected || !reflect.DeepEqual(got, want) {
						t.Fatalf("词库预览发生变化：文件=%q，查询=%q，页码=%d，总数=%d/%d，结果=%+v，期望=%+v", name, query, page, total, expected, got, want)
					}
				}
			}
		}
	}
}

func TestDictionaryRowsLineScanAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, line, query string }{
		{"matching", "水杉\tshui'shan\t100\n", "水杉"},
		{"uppercase_preview", "SYNTHETIC\tWORD\t100\n", ""},
		{"unicode_preview", "水杉İΣ\tABC\t100\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat(tc.line, 50000)
			var rows []dictionaryRow
			total := 0
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					rows, total = dictionaryRows("custom/words.txt", text, tc.query, 2)
				}
			})
			if total != 50000 || len(rows) != 100 || rows[0].Line != 101 || rows[99].Line != 200 {
				t.Fatal("大词库预览的行号、分页或总数发生变化")
			}
			if allocated := result.AllocedBytesPerOp(); allocated > 64<<10 {
				t.Fatalf("五万行词库预览分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func TestDictionaryRowsCommentQueryBudget(t *testing.T) {
	var text strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&text, "# %s %05d\n", strings.Repeat("注释内容 ", 500), i)
	}
	input := text.String()
	previous := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_, _ = previousDictionaryRows("custom/words.txt", input, "needle", 1)
		}
	})
	current := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			_, _ = dictionaryRows("custom/words.txt", input, "needle", 1)
		}
	})
	t.Logf("当前 %s，原实现 %s", current, previous)
	if previous.N == 0 || current.N == 0 || current.NsPerOp() >= previous.NsPerOp()*12/10 {
		t.Fatalf("注释行查询没有达到预期加速：当前 %s，原实现 %s", current, previous)
	}
}

func BenchmarkDictionaryRowsEmptyQuery(b *testing.B) {
	for _, tc := range []struct{ name, line string }{
		{"ascii", "SYNTHETIC\tWORD\t100\n"},
		{"unicode", "水杉İΣ\tABC\t100\n"},
	} {
		for _, size := range []int{100, 50000} {
			b.Run(fmt.Sprintf("%s/%d", tc.name, size), func(b *testing.B) {
				text := strings.Repeat(tc.line, size)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_, _ = dictionaryRows("custom/words.txt", text, "", 1)
				}
			})
		}
	}
}

func BenchmarkDictionaryRowsLineScan(b *testing.B) {
	for _, tc := range []struct{ name, text, query string }{
		{"small", strings.Repeat("水杉\tshui'shan\t100\n", 100), "水杉"},
		{"large", strings.Repeat("水杉\tshui'shan\t100\n", 50000), "水杉"},
		{"no_matches", strings.Repeat("水杉\tshui'shan\t100\n", 50000), "missing"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for _, implementation := range []struct {
				name string
				fn   func(string, string, string, int) ([]dictionaryRow, int)
			}{{"previous", previousDictionaryRows}, {"current", dictionaryRows}} {
				b.Run(implementation.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_, _ = implementation.fn("custom/words.txt", tc.text, tc.query, 1)
					}
				})
			}
		})
	}
}
