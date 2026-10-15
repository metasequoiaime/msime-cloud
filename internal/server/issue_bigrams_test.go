package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

func previousTitleBigrams(title string) map[string]bool {
	set := map[string]bool{}
	var previous rune
	for _, current := range strings.ToLower(title) {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			previous = 0
			continue
		}
		if previous != 0 {
			set[string([]rune{previous, current})] = true
		}
		previous = current
	}
	return set
}

func previousBigramSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for key := range a {
		if b[key] {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(a)+len(b))
}

func TestTitleBigramsMatchesPrevious(t *testing.T) {
	titles := []string{"", "a", "abab", "Win11 下 Word 候选窗偏移", "Win10 下 Word 候选窗偏移", "a-b a_b a b", "İ K Σς", "ＡＢ１２", "\xff水\xe4\xb8杉", "𠀀𠀁𠀂", "e\u0301x", "水\x00杉"}
	random := rand.New(rand.NewSource(71))
	units := []string{"a", "A", "B", "1", "２", "水", "杉", "𠀀", "𠀁", "Σ", "ς", "İ", "K", " ", "_", "-", "\n", "\x00", "\u0301", "😀", "\xff", "\xe4\xb8"}
	for range 500 {
		var title strings.Builder
		for range random.Intn(100) {
			title.WriteString(units[random.Intn(len(units))])
		}
		titles = append(titles, title.String())
	}
	for i, title := range titles {
		other := titles[(i+1)%len(titles)]
		actual, expected := titleBigrams(title), previousTitleBigrams(title)
		if len(actual) != len(expected) {
			t.Fatalf("标题二元组数量发生变化：标题=%q，结果=%d，期望=%d", title, len(actual), len(expected))
		}
		for pair := range actual {
			if key := string(pair[:]); !expected[key] {
				t.Fatalf("标题出现额外二元组：标题=%q，二元组=%q", title, key)
			}
		}
		for _, comparison := range []string{other, title, strings.ToLower(title), "水杉 Word 候选窗口"} {
			got := bigramSimilarity(actual, titleBigrams(comparison))
			want := previousBigramSimilarity(expected, previousTitleBigrams(comparison))
			if got != want {
				t.Fatalf("标题相似度发生变化：标题=%q，对照=%q，结果=%g，期望=%g", title, comparison, got, want)
			}
		}
	}
}

func TestTitleBigramsAllocationBudget(t *testing.T) {
	title := strings.Repeat("Windows Word 候选窗口位置偏移", 32)
	count := 0
	allocations := testing.AllocsPerRun(100, func() {
		count = len(titleBigrams(title))
	})
	if count != len(previousTitleBigrams(title)) {
		t.Fatal("标题二元组数量发生变化")
	}
	// 为不同平台的 map 扩容留出余量，同时排除为每个字符对分配字符串的旧实现。
	if allocations > 16 {
		t.Fatalf("标题二元组分配 %.1f 次，超过 16 次预算", allocations)
	}
}

func BenchmarkTitleBigrams(b *testing.B) {
	for _, tc := range []struct{ name, title string }{
		{"chinese", "多显示器下候选窗口位置偏移"},
		{"mixed", "Windows 11 下 Word 候选窗口位置偏移"},
		{"repeated", strings.Repeat("Windows Word 候选窗口位置偏移", 32)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("previous", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = previousTitleBigrams(tc.title)
				}
			})
			b.Run("current", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = titleBigrams(tc.title)
				}
			})
		})
	}
}
