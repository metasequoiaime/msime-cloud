package account

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// 冻结原有的完整拼接和 rune 转换，核对截断、分隔符和非法 UTF-8 的兼容性。
func previousSensitiveFlagText(patterns []string) string {
	text := "命中敏感词：「" + strings.Join(patterns, "」「") + "」"
	if utf8.RuneCountInString(text) > 500 {
		text = string([]rune(text)[:499]) + "…"
	}
	return text
}

func TestSensitiveFlagPrefixCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		want     string
	}{
		{"Nil", nil, "命中敏感词：「」"},
		{"Empty", []string{}, "命中敏感词：「」"},
		{"EmptyPattern", []string{""}, "命中敏感词：「」"},
		{"TwoPatterns", []string{"合成词甲", "合成词乙"}, "命中敏感词：「合成词甲」「合成词乙」"},
		{"ExactLimit", []string{strings.Repeat("水", 492)}, "命中敏感词：「" + strings.Repeat("水", 492) + "」"},
		{"OverLimit", []string{strings.Repeat("水", 493)}, "命中敏感词：「" + strings.Repeat("水", 492) + "…"},
		{"LargeEmojiPattern", []string{strings.Repeat("😀", 600)}, "命中敏感词：「" + strings.Repeat("😀", 492) + "…"},
		{"CutBeforeSeparator", []string{strings.Repeat("水", 492), "尾"}, "命中敏感词：「" + strings.Repeat("水", 492) + "…"},
		{"CutInSeparator", []string{strings.Repeat("水", 491), "尾"}, "命中敏感词：「" + strings.Repeat("水", 491) + "」…"},
		{"CutAfterSeparator", []string{strings.Repeat("水", 490), "尾词"}, "命中敏感词：「" + strings.Repeat("水", 490) + "」「…"},
		{"InvalidShort", []string{"\xff\xfe"}, "命中敏感词：「\xff\xfe」"},
		{"InvalidExactLimit", []string{strings.Repeat("水", 490) + "\xff\xfe"}, "命中敏感词：「" + strings.Repeat("水", 490) + "\xff\xfe」"},
		{"InvalidTruncated", []string{strings.Repeat("水", 490) + "\xff\xfe尾"}, "命中敏感词：「" + strings.Repeat("水", 490) + "\ufffd\ufffd…"},
		{"BrokenAtCut", []string{strings.Repeat("水", 491) + "\xf0\x9f\x98尾"}, "命中敏感词：「" + strings.Repeat("水", 491) + "\ufffd…"},
		{"InvalidAfterCut", []string{strings.Repeat("水", 492) + "\xff\xfe"}, "命中敏感词：「" + strings.Repeat("水", 492) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sensitiveFlagText(tc.patterns); got != tc.want {
				t.Fatalf("提示=%q，期望=%q", got, tc.want)
			}
		})
	}
	random := rand.New(rand.NewSource(695))
	atoms := []string{"a", "水", "😀", "\ufffd", "\xff", "\xfe", "\xf0\x9f", "\x00", "\t", "\n", "\r", "\u3000", "\u00a0", "」「"}
	for range 1000 {
		patterns := make([]string, random.Intn(12))
		for i := range patterns {
			var pattern strings.Builder
			for range random.Intn(200) {
				pattern.WriteString(atoms[random.Intn(len(atoms))])
			}
			patterns[i] = pattern.String()
		}
		if got, want := sensitiveFlagText(patterns), previousSensitiveFlagText(patterns); got != want {
			t.Fatalf("提示=%q，原提示=%q", got, want)
		}
	}
}

func TestSensitiveFlagPrefixAllocationBudget(t *testing.T) {
	patterns := make([]string, 1000)
	for i := range patterns {
		patterns[i] = "合成审核规则" + strings.Repeat("水", 190) + strconv.Itoa(i)
	}
	for _, input := range [][]string{{"合成词甲", "合成词乙"}, patterns} {
		want := previousSensitiveFlagText(input)
		allocs := testing.AllocsPerRun(100, func() {
			if got := sensitiveFlagText(input); got != want {
				t.Fatalf("提示=%q，原提示=%q", got, want)
			}
		})
		if allocs > 1 {
			t.Errorf("规则数=%d，分配=%.0f，期望最多 1 次", len(input), allocs)
		}
	}
}

var sensitiveFlagPrefixResult string

func BenchmarkSensitiveFlagPrefix(b *testing.B) {
	for _, tc := range []struct {
		name     string
		patterns []string
	}{
		{"Empty", nil},
		{"SingleASCII", []string{"synthetic"}},
		{"SingleUnicode", []string{"合成词"}},
		{"SingleMaximumUnicode", []string{strings.Repeat("水", 200)}},
		{"TwoUnicode", []string{"合成词甲", "合成词乙"}},
		{"TwoLongUnicode", []string{strings.Repeat("水", 100), strings.Repeat("杉", 100)}},
		{"ManyUnicode", func() []string {
			patterns := make([]string, 1000)
			for i := range patterns {
				patterns[i] = "合成审核规则" + strings.Repeat("水", 190) + strconv.Itoa(i)
			}
			return patterns
		}()},
		{"InvalidPrefix", []string{strings.Repeat("水", 490) + "\xff\xfe尾"}},
	} {
		for _, impl := range []struct {
			name string
			flag func([]string) string
		}{{"Previous", previousSensitiveFlagText}, {"Current", sensitiveFlagText}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					sensitiveFlagPrefixResult = impl.flag(tc.patterns)
				}
			})
		}
	}
}
