package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// previousQQWordRejection 保留先拆分全部拼音的旧实现，用于核对错误文案和分配。
func previousQQWordRejection(e wordSubmissionEntry, rejected wordRejection) string {
	w := e.Word
	syllables := strings.Split(e.Pinyin, "'")
	switch rejected.Code {
	case "invalid_pinyin":
		return "「" + w + "」的拼音只能写字母，音节之间空一格，比如 @我 发「加词 扛把子 kang ba zi」。"
	case "syllable_count_mismatch":
		return fmt.Sprintf("「%s」是 %d 个字，拼音却是 %d 个音节，对一下再发。", w, utf8.RuneCountInString(w), len(syllables))
	case "invalid_syllable":
		for _, syllable := range syllables {
			if !pinyinSyllables[syllable] {
				return "「" + w + "」的拼音里「" + syllable + "」不是有效的拼音（ü 写作 v，如 lv）。"
			}
		}
	}
	// 其他原因照用官网的文案，去掉开头重复的词语（「“扛把子”重复填写了」）。
	return "「" + w + "」" + strings.TrimPrefix(rejected.Reason, "“"+w+"”") + "。"
}

func checkQQWordRejectionScan(t testing.TB, entry wordSubmissionEntry, rejected wordRejection) {
	t.Helper()
	if got, want := qqWordRejection(entry, rejected), previousQQWordRejection(entry, rejected); got != want {
		t.Fatalf("错误文案变化：词条=%+v，拒绝=%+v，结果=%q，期望=%q", entry, rejected, got, want)
	}
}

func TestQQWordRejectionScanCompatibility(t *testing.T) {
	codes := []string{"invalid_pinyin", "syllable_count_mismatch", "invalid_syllable", "word_required", "invalid_word", "word_too_long", "pinyin_required", "duplicate_entry", "unknown", ""}
	for _, word := range []string{"", "水杉", "〇", "扛把子", "𠀀", "水\xff杉"} {
		for _, pinyin := range []string{"", "a", "shui'shan", "kang'ba'zii", "zz'a'xx", "a'", "'a", "a''ba", "汉'字", "a\xff'ba", strings.Repeat("a'", 500)} {
			for _, code := range codes {
				checkQQWordRejectionScan(t, wordSubmissionEntry{Word: word, Pinyin: pinyin}, wordRejection{Code: code, Reason: "“" + word + "”测试原因"})
			}
		}
	}
	random := rand.New(rand.NewSource(1062))
	units := []string{"a", "ba", "zz", "'", "'a", "字", "\xff", "ā", " ", "\x00"}
	for range 2000 {
		var pinyin strings.Builder
		for range random.Intn(100) {
			pinyin.WriteString(units[random.Intn(len(units))])
		}
		entry := wordSubmissionEntry{Word: "水杉", Pinyin: pinyin.String()}
		checkQQWordRejectionScan(t, entry, wordRejection{Code: codes[random.Intn(len(codes))], Reason: "合成原因"})
		for _, rejected := range validateWordEntries([]wordSubmissionEntry{entry}) {
			checkQQWordRejectionScan(t, entry, rejected)
		}
	}
}

func FuzzQQWordRejectionScan(f *testing.F) {
	for _, seed := range []struct{ word, pinyin, code, reason string }{
		{"水杉", "shui'shan", "invalid_syllable", "其他原因"},
		{"扛把子", "kang'ba", "syllable_count_mismatch", ""},
		{"水杉", "'a''", "invalid_syllable", ""},
		{"水\xff杉", "a\xff'ba", "unknown", "“水\xff杉”原因"},
	} {
		f.Add(seed.word, seed.pinyin, seed.code, seed.reason)
	}
	f.Fuzz(func(t *testing.T, word, pinyin, code, reason string) {
		checkQQWordRejectionScan(t, wordSubmissionEntry{Word: word, Pinyin: pinyin}, wordRejection{Code: code, Reason: reason})
	})
}

func qqWordRejectionScanSamples() []struct {
	name  string
	entry wordSubmissionEntry
	code  string
} {
	return []struct {
		name  string
		entry wordSubmissionEntry
		code  string
	}{
		{"short_invalid", wordSubmissionEntry{Word: "扛把子", Pinyin: "kang4'ba3'zi3"}, "invalid_pinyin"},
		{"long_invalid", wordSubmissionEntry{Word: "水杉", Pinyin: strings.Repeat("a'", 699) + "a"}, "invalid_pinyin"},
		{"word_too_long", wordSubmissionEntry{Word: strings.Repeat("水", 17), Pinyin: strings.Repeat("a'", 699) + "a"}, "word_too_long"},
		{"count_mismatch", wordSubmissionEntry{Word: "水杉", Pinyin: strings.Repeat("a'", 98) + "a"}, "syllable_count_mismatch"},
		{"first_invalid", wordSubmissionEntry{Word: "水杉", Pinyin: "zz'" + strings.Repeat("a'", 97) + "a"}, "invalid_syllable"},
		{"last_invalid", wordSubmissionEntry{Word: "水杉", Pinyin: strings.Repeat("a'", 98) + "zz"}, "invalid_syllable"},
	}
}

func TestQQWordRejectionScanAllocationBudget(t *testing.T) {
	for _, sample := range qqWordRejectionScanSamples() {
		t.Run(sample.name, func(t *testing.T) {
			if utf8.RuneCountInString("加词 "+sample.entry.Word+" "+sample.entry.Pinyin) > qqBotMaxRunes {
				t.Fatal("回归输入超过 QQ 消息的实际字数上限")
			}
			parsed, lettered := qqParseWords(sample.entry.Word + " " + sample.entry.Pinyin)
			if lettered || len(parsed) != 1 || parsed[0] != sample.entry {
				t.Fatalf("回归输入与实际加词解析结果不符：词条=%+v，夹杂字母=%v", parsed, lettered)
			}
			rejected := validateWordEntries([]wordSubmissionEntry{sample.entry})
			if len(rejected) != 1 || rejected[0].Code != sample.code {
				t.Fatalf("回归输入未触发预期拒绝：%+v", rejected)
			}
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqWordRejection(sample.entry, rejected[0])
				}
			})
			if want := previousQQWordRejection(sample.entry, rejected[0]); got != want {
				t.Fatalf("错误文案=%q，期望=%q", got, want)
			}
			if allocated := result.AllocedBytesPerOp(); allocated > 512 {
				t.Fatalf("错误文案分配 %s，超过512字节预算", result.MemString())
			}
		})
	}
}

func BenchmarkQQWordRejectionScan(b *testing.B) {
	for _, sample := range qqWordRejectionScanSamples() {
		rejected := validateWordEntries([]wordSubmissionEntry{sample.entry})[0]
		for _, impl := range []struct {
			name string
			run  func(wordSubmissionEntry, wordRejection) string
		}{
			{"previous", previousQQWordRejection}, {"current", qqWordRejection},
		} {
			b.Run(sample.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(sample.entry, rejected)
				}
			})
		}
	}
}
