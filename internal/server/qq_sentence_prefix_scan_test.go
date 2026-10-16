package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// 固定本项优化前的前缀 rune 切片流程，校验截断边界、编码和开销。
func previousQQTruncateSentencePrefix(text string, limit int) string {
	end := len(text)
	if limit >= 0 {
		end = qqRuneEnd(text, limit)
		if end == len(text) {
			return text
		}
	}
	kept := []rune(text[:end])[:max(limit-1, 0)]
	for i := len(kept) - 1; i >= limit/2; i-- {
		if strings.ContainsRune(qqSentenceBreaks, kept[i]) {
			kept = kept[:i+1]
			break
		}
	}
	return strings.TrimRightFunc(string(kept), unicode.IsSpace) + "…"
}

func TestQQTruncateSentencePrefixCompatibility(t *testing.T) {
	texts := []string{"", "合成正文。下一句还有内容", "\xff\xfe水杉\xe4\xb8", "a\ufffd\u3000 \t\n", "a𐐀😀b\u0301", strings.Repeat("水", 600) + "\xff", strings.Repeat("\xff", 1000), strings.Repeat("合成正文。", 1000)}

	rng := rand.New(rand.NewSource(685))
	atoms := []string{"水杉", "A", " ", "\t", "\r", "\n", "\u3000", "\u00a0", "\u0085", "\u200b", "。", "！", "？", "；", "𐐀", "\ufffd", "\xff", "\xe4\xb8", "\xed\xa0\x80"}
	for range 1000 {
		var text strings.Builder
		for range rng.Intn(500) {
			text.WriteString(atoms[rng.Intn(len(atoms))])
		}
		texts = append(texts, text.String())
	}
	run := func(fn func(string, int) string, text string, limit int) (result string, panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		return fn(text, limit), false
	}
	check := func(text string, limit int) {
		t.Helper()
		got, panicGot := run(qqTruncateSentence, text, limit)
		want, panicWant := run(previousQQTruncateSentencePrefix, text, limit)
		if got != want || panicGot != panicWant {
			t.Fatalf("文本=%q，上限=%d，输出=%q/%t，期望=%q/%t", text, limit, got, panicGot, want, panicWant)
		}
	}
	for limit := 2; limit <= 64; limit++ {
		for _, position := range []int{limit/2 - 1, limit / 2, limit - 2, limit - 1, limit} {
			for _, ending := range []string{"。", "！", "？", "；", "\n", ".", ";", "!", "?"} {
				check(strings.Repeat("水", position)+ending+strings.Repeat("尾", limit+2), limit)
			}
		}
	}
	maxInt := int(^uint(0) >> 1)
	for _, text := range texts {
		count := utf8.RuneCountInString(text)
		for _, limit := range []int{-maxInt - 1, -4, -3, -2, -1, 0, 1, 2, 3, count / 2, count - 1, count, count + 1, 300, 600, 3000, maxInt} {
			check(text, limit)
		}
	}
}

func TestQQTruncateSentencePrefixAllocations(t *testing.T) {
	for _, tc := range []struct {
		text  string
		limit int
	}{
		{strings.Repeat("A", 10000), 600},
		{strings.Repeat("水杉", 5000), 600},
		{strings.Repeat("水杉", 5000), 3000},
		{strings.Repeat("水", 400) + "。" + strings.Repeat("尾", 10000), 600},
		{strings.Repeat("\xff", 10000), 600},
		{strings.Repeat("水\xff", 5000), 600},
	} {
		var got string
		allocated := testing.AllocsPerRun(10, func() { got = qqTruncateSentence(tc.text, tc.limit) })
		if got != previousQQTruncateSentencePrefix(tc.text, tc.limit) {
			t.Fatal("截断结果变化")
		}
		if allocated > 1 {
			t.Fatalf("%d 字节正文按 %d 字截断分配 %g 次，期望至多一次结果分配", len(tc.text), tc.limit, allocated)
		}
	}
}

var qqSentencePrefixResult string

func BenchmarkQQTruncateSentencePrefix(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		limit      int
	}{
		{"ASCII", strings.Repeat("A", 10000), 600},
		{"Chinese", strings.Repeat("水杉", 5000), 600},
		{"IssueBody", strings.Repeat("水杉", 5000), 3000},
		{"SentenceBreak", strings.Repeat("水", 400) + "。" + strings.Repeat("尾", 10000), 600},
		{"InvalidUTF8", strings.Repeat("\xff", 10000), 600},
		{"MixedUTF8", strings.Repeat("水\xff", 5000), 600},
		{"NoTruncation", strings.Repeat("水", 600), 600},
	} {
		for _, impl := range []struct {
			name     string
			truncate func(string, int) string
		}{
			{"Previous", previousQQTruncateSentencePrefix}, {"Current", qqTruncateSentence},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqSentencePrefixResult = impl.truncate(tc.text, tc.limit)
				}
			})
		}
	}
}
