package server

import (
	"bytes"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// 保留原有短 ASCII 快路径，比较 Unicode 扩展前后的开销。
func previousDictionaryLineContainsLower(line, query string) bool {
	// 原流程遇到 Unicode 或长行时交给标准库。
	var lower [256]byte
	if len(line) > len(lower) {
		return strings.Contains(strings.ToLower(line), query)
	}
	for i := range len(line) {
		c := line[i]
		if c >= utf8.RuneSelf {
			return strings.Contains(strings.ToLower(line), query)
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	return bytes.Contains(lower[:len(line)], []byte(query))
}

func TestDictionaryUnicodeLineContainsLower(t *testing.T) {
	lines := []string{"", "水杉İΣſKẞ\tWORD\t100", "aȺȾİKΣẞ𐐀z", "\xff\xfeABC\xe4\xb8", "\ufffdABC", "\u200bＡＢＣ"}
	for _, unit := range []string{"A", "水", "Ⱥ", "𐐀", "\xff"} {
		for _, size := range []int{1, 63, 64, 85, 86, 127, 128, 255, 256, 257} {
			lines = append(lines, strings.Repeat(unit, size))
		}
	}
	rng := rand.New(rand.NewSource(679))
	for range 1000 {
		raw := make([]byte, rng.Intn(300))
		_, _ = rng.Read(raw)
		lines = append(lines, string(raw))
	}
	for _, line := range lines {
		lower := strings.ToLower(line)
		queries := []string{"", "WORD", "word", "水", "i", "σ", "ς", "s", "ſ", "k", "ß", "ss", "ⱥ", "ⱦ", "𐐨", "\xff", "\ufffd", "missing"}
		// 查询按原流程预先转小写；完整字节子串也覆盖非法编码边界。
		for range 5 {
			start := rng.Intn(len(lower) + 1)
			end := start + rng.Intn(len(lower)-start+1)
			queries = append(queries, lower[start:end])
		}
		for _, query := range queries {
			query = strings.ToLower(query)
			if got, want := dictionaryLineContainsLower(line, query), strings.Contains(lower, query); got != want {
				t.Fatalf("行=%q，查询=%q，匹配=%t，期望=%t", line, query, got, want)
			}
		}
	}
}

func TestDictionaryRowsUnicodeQueryMemoryBudget(t *testing.T) {
	for _, tc := range []struct {
		name, line, query string
		matches           bool
	}{
		{"中文混合英文命中", "水杉İΣ\tWORD\t100\n", "WORD", true},
		{"中文混合英文未命中", "水杉İΣ\tWORD\t100\n", "missing", false},
		{"小写编码扩长", "ȺȾ\tWORD\t100\n", "ⱥⱦ", true},
		{"补充平面大小写", "𐐀\tWORD\t100\n", "𐐨", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat(tc.line, 50000)
			var rows []dictionaryRow
			var total int
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					rows, total = dictionaryRows("custom/words.txt", text, tc.query, 2)
				}
			})
			want, expected := previousDictionaryRows("custom/words.txt", text, tc.query, 2)
			if total != expected || !reflect.DeepEqual(rows, want) {
				t.Fatal("查询结果、分页或行号发生变化")
			}
			if tc.matches && (total != 50000 || len(rows) != 100 || rows[0].Line != 101 || rows[99].Line != 200) {
				t.Fatal("缺少预期的匹配结果")
			}
			t.Logf("%s; %s", result, result.MemString())
			// 保留返回当前页的分配预算，排除逐行小写副本。
			if allocated := result.AllocedBytesPerOp(); allocated > 64<<10 {
				t.Fatalf("五万行 Unicode 查询分配 %d 字节，超过 64 KiB 预算", allocated)
			}
		})
	}
}

func TestDictionaryUnicodeLineQueryAllocations(t *testing.T) {
	for _, line := range []string{"水杉İΣ\tWORD\t100", strings.Repeat("Ⱥ", 128), strings.Repeat("\xff", 256)} {
		var got bool
		if allocated := testing.AllocsPerRun(100, func() { got = dictionaryLineContainsLower(line, "missing") }); allocated != 0 {
			t.Fatalf("%d 字节的短行查询分配 %g 次，期望零分配", len(line), allocated)
		}
		if got {
			t.Fatal("未命中的查询返回了匹配")
		}
	}
}

var dictionaryUnicodeQueryResult bool

func BenchmarkDictionaryUnicodeLineQuery(b *testing.B) {
	for _, tc := range []struct{ name, line, query string }{
		{"ASCII", "SYNTHETIC\tWORD\t100", "word"},
		{"Mixed", "水杉İΣ\tWORD\t100", "word"},
		{"Chinese", "水杉\tshui'shan\t100", "missing"},
		{"Expanding", strings.Repeat("Ⱥ", 128), "missing"},
		{"Invalid", strings.Repeat("\xff", 256), "missing"},
		{"Long", strings.Repeat("水杉İΣ", 1000), "missing"},
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
