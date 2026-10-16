package server

import (
	"strings"
	"testing"
)

func TestDictLineColumnsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, line, first, second string
	}{
		{"空行", "", "", ""},
		{"仅分隔符与空白", " \t\t\r\n", "", ""},
		{"单列", "水杉", "水杉", ""},
		{"单列末尾分隔符", "水杉\t", "水杉", ""},
		{"前导分隔符按原规则修剪", "\t水杉\t拼音", "水杉", "拼音"},
		{"前导空列按原规则修剪", " \t \t水杉", "水杉", ""},
		{"两列", "水杉\tshui'shan", "水杉", "shui'shan"},
		{"忽略权重", "水杉\tshui'shan\t5000", "水杉", "shui'shan"},
		{"空第二列", "水杉\t\t5000", "水杉", ""},
		{"多余列", "水杉\tshui'shan\t5000\t忽略\t", "水杉", "shui'shan"},
		{"ASCII空白", " \t水杉 \t shui'shan \t5000\r\n", "水杉", "shui'shan"},
		{"Unicode空白", "　水杉\u00a0\t　shui'shan　\t5000\r\n", "水杉", "shui'shan"},
		{"列内CR", "word\r\tsecond\r\tignored", "word", "second"},
		{"保留列内换行", "\nword\tsecond\nignored\t999\n", "word", "second\nignored"},
		{"保留BOM", "\ufeffword\tsecond", "\ufeffword", "second"},
		{"保留无效UTF8", "\xff \t \xe4\xb8 \tignored", "\xff", "\xe4\xb8"},
		{"保留NUL", "word\x00\tsecond\x00\tignored", "word\x00", "second\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := dictLineColumns(tc.line)
			if first != tc.first || second != tc.second {
				t.Fatalf("两列=%q/%q，期望 %q/%q", first, second, tc.first, tc.second)
			}
		})
	}
}

func TestDictLineColumnsZeroAllocations(t *testing.T) {
	for _, tc := range []struct{ name, line, first, second string }{
		{"空行", "", "", ""},
		{"单列", "水杉", "水杉", ""},
		{"正常词条", "水杉\tshui'shan\t5000", "水杉", "shui'shan"},
		{"大量忽略列", "水杉\tshui'shan" + strings.Repeat("\t忽略", 32768), "水杉", "shui'shan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first, second string
			allocations := testing.AllocsPerRun(20, func() { first, second = dictLineColumns(tc.line) })
			if first != tc.first || second != tc.second {
				t.Fatalf("两列=%q/%q，期望 %q/%q", first, second, tc.first, tc.second)
			}
			if allocations != 0 {
				t.Fatalf("读取前两列分配 %g 次，期望零分配", allocations)
			}
		})
	}
}

func BenchmarkDictLineColumns(b *testing.B) {
	for _, tc := range []struct{ name, line string }{
		{"两列", "水杉\tshui'shan"},
		{"带权重", "水杉\tshui'shan\t5000\r"},
		{"大量忽略列", "水杉\tshui'shan" + strings.Repeat("\t忽略", 32768)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				first, second := dictLineColumns(tc.line)
				if first != "水杉" || second != "shui'shan" {
					b.Fatal("读取前两列结果不符合预期")
				}
			}
		})
	}
}
