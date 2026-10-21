package server

import (
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func previousValidateEnglishEntries(entries []englishSubmissionEntry) []wordRejection {
	rejected := []wordRejection{}
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		reject := func(code, reason string) { rejected = append(rejected, wordRejection{i, code, reason}) }
		e.Display = strings.TrimSpace(e.Display)
		switch {
		case e.Word == "":
			reject("word_required", "请填写英文单词")
		case strings.IndexFunc(e.Word, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0:
			reject("invalid_word", "单词是输入时键入的编码，只能包含小写英文字母 a–z，不能有大写、数字、空格或符号")
		case len(e.Word) > englishWordMaxBytes:
			reject("word_too_long", "单词最多 64 个字母")
		case e.Display == "":
			reject("display_required", "请填写候选中显示的词形")
		case !plainText(e.Display):
			reject("invalid_display", "显示词形不能包含制表符、换行或其他控制字符")
		case utf8.RuneCountInString(e.Display) > englishDisplayMaxChars:
			reject("display_too_long", "显示词形最多 64 个字符")
		case seen[e.Word+"\t"+e.Display]:
			reject("duplicate_entry", "“"+e.Display+"”重复填写了")
		default:
			seen[e.Word+"\t"+e.Display] = true
		}
	}
	return rejected
}

func previousValidateTranslationEntries(entries []translationSubmissionEntry) []wordRejection {
	rejected := []wordRejection{}
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		reject := func(code, reason string) { rejected = append(rejected, wordRejection{i, code, reason}) }
		e.Source, e.Gloss = strings.TrimSpace(e.Source), strings.TrimSpace(e.Gloss)
		switch {
		case e.Source == "":
			reject("source_required", "请填写原词")
		case !plainText(e.Source):
			reject("invalid_source", "原词不能包含制表符、换行或其他控制字符")
		case strings.HasPrefix(e.Source, "#"):
			reject("invalid_source", "原词不能以 # 开头")
		case utf8.RuneCountInString(e.Source) > translationSourceChars:
			reject("source_too_long", "原词最多 64 个字符")
		case e.Gloss == "":
			reject("gloss_required", "请填写译文")
		case !plainText(e.Gloss):
			reject("invalid_gloss", "译文不能包含制表符、换行或其他控制字符")
		case utf8.RuneCountInString(e.Gloss) > translationGlossChars:
			reject("gloss_too_long", "译文最多 200 个字符")
		case seen[e.Source+"\t"+e.Gloss]:
			reject("duplicate_entry", "“"+e.Source+"”的这条翻译重复填写了")
		default:
			seen[e.Source+"\t"+e.Gloss] = true
		}
	}
	return rejected
}

func wordSubmissionDedupFixtures(size int, long bool) ([]wordSubmissionEntry, []englishSubmissionEntry, []translationSubmissionEntry) {
	words := make([]wordSubmissionEntry, size)
	english := make([]englishSubmissionEntry, size)
	translations := make([]translationSubmissionEntry, size)
	for i := range size {
		word, pinyin := string(rune('一'+i)), "yi"
		code, display := string(rune('a'+i%26)), string(rune('A'+i%26))
		source, gloss := string(rune('一'+i)), "synthetic"
		if long {
			word += strings.Repeat("好", 15)
			pinyin += strings.Repeat("'hao", 15)
			code += strings.Repeat("a", 63)
			display += strings.Repeat("A", 63)
			source += strings.Repeat("好", 63)
			gloss = strings.Repeat("x", 200)
		}
		words[i] = wordSubmissionEntry{word, pinyin}
		english[i] = englishSubmissionEntry{code, display}
		translations[i] = translationSubmissionEntry{source, gloss}
	}
	return words, english, translations
}

func TestWordSubmissionDedupAllocationBudget(t *testing.T) {
	for _, long := range []bool{false, true} {
		length := "short"
		if long {
			length = "long"
		}
		for _, size := range []int{1, wordSubmissionMaxEntries} {
			words, english, translations := wordSubmissionDedupFixtures(size, long)
			for _, tc := range []struct {
				name string
				run  func() []wordRejection
			}{
				{"words", func() []wordRejection { return validateWordEntries(words) }},
				{"english", func() []wordRejection { return validateEnglishEntries(english) }},
				{"translations", func() []wordRejection { return validateTranslationEntries(translations) }},
			} {
				t.Run(tc.name+"/"+length+"/"+strconv.Itoa(size), func(t *testing.T) {
					var rejected []wordRejection
					allocations := testing.AllocsPerRun(100, func() { rejected = tc.run() })
					if len(rejected) != 0 {
						t.Fatalf("有效词条被拒绝：%#v", rejected)
					}
					budget := 0.0
					if allocations > budget {
						t.Fatalf("校验 %d 条记录分配 %.0f 次，超过 %.0f 次预算", size, allocations, budget)
					}
				})
			}
		}
	}
}

func TestWordSubmissionDedupCompatibility(t *testing.T) {
	random := rand.New(rand.NewSource(1133))
	codes := []string{"", "a", "ab", "a\tb", "github", "GitHub", "\xff", strings.Repeat("a", 64), strings.Repeat("a", 65)}
	texts := []string{"", " ", " A ", "A", "AB", " A\tB ", "\xff", "\u200b", "苹果", " apple ", "#source", strings.Repeat("好", 64), strings.Repeat("好", 65), strings.Repeat("x", 200), strings.Repeat("x", 201)}
	for range 2000 {
		english := []englishSubmissionEntry{{"github", " GitHub "}, {"github", "GitHub"}, {"github", "Github"}}
		translations := []translationSubmissionEntry{{" 苹果 ", " apple "}, {"苹果", "apple"}, {"苹果", "Apple"}, {"apple", "苹果"}}
		for range random.Intn(wordSubmissionMaxEntries) {
			english = append(english, englishSubmissionEntry{codes[random.Intn(len(codes))], texts[random.Intn(len(texts))]})
			translations = append(translations, translationSubmissionEntry{texts[random.Intn(len(texts))], texts[random.Intn(len(texts))]})
		}
		english = append(english, english[random.Intn(len(english))])
		translations = append(translations, translations[random.Intn(len(translations))])
		oldEnglish := slices.Clone(english)
		oldTranslations := slices.Clone(translations)
		if got, want := validateEnglishEntries(english), previousValidateEnglishEntries(oldEnglish); !reflect.DeepEqual(got, want) || !slices.Equal(english, oldEnglish) {
			t.Fatalf("英文词条的去重、错误顺序或原地裁剪发生变化：结果=%#v，期望=%#v", got, want)
		}
		if got, want := validateTranslationEntries(translations), previousValidateTranslationEntries(oldTranslations); !reflect.DeepEqual(got, want) || !slices.Equal(translations, oldTranslations) {
			t.Fatalf("翻译词条的去重、错误顺序或原地裁剪发生变化：结果=%#v，期望=%#v", got, want)
		}
	}
}

func TestWordSubmissionDedupLargeBatchCompatibility(t *testing.T) {
	for _, long := range []bool{false, true} {
		words, english, translations := wordSubmissionDedupFixtures(64, long)
		words = append(words, words[0], words[63])
		english = append(english, english[0], english[63])
		translations = append(translations, translations[0], translations[63])
		if got, want := validateWordEntries(words), previousValidateWordEntries(words); !reflect.DeepEqual(got, want) {
			t.Fatalf("大批量词语去重发生变化：结果=%#v，期望=%#v", got, want)
		}
		if got, want := validateEnglishEntries(english), previousValidateEnglishEntries(slices.Clone(english)); !reflect.DeepEqual(got, want) {
			t.Fatalf("大批量英文去重发生变化：结果=%#v，期望=%#v", got, want)
		}
		if got, want := validateTranslationEntries(translations), previousValidateTranslationEntries(slices.Clone(translations)); !reflect.DeepEqual(got, want) {
			t.Fatalf("大批量翻译去重发生变化：结果=%#v，期望=%#v", got, want)
		}
	}
}

func BenchmarkWordSubmissionDedup(b *testing.B) {
	for _, long := range []bool{false, true} {
		length := "short"
		if long {
			length = "long"
		}
		for _, size := range []int{1, wordSubmissionMaxEntries} {
			words, english, translations := wordSubmissionDedupFixtures(size, long)
			for _, tc := range []struct {
				name string
				run  func() []wordRejection
			}{
				{"words", func() []wordRejection { return validateWordEntries(words) }},
				{"english", func() []wordRejection { return validateEnglishEntries(english) }},
				{"translations", func() []wordRejection { return validateTranslationEntries(translations) }},
			} {
				b.Run(tc.name+"/"+length+"/"+strconv.Itoa(size), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = tc.run()
					}
				})
			}
		}
	}
}

func BenchmarkWordSubmissionDedupSharedPrefix(b *testing.B) {
	english := make([]englishSubmissionEntry, wordSubmissionMaxEntries)
	translations := make([]translationSubmissionEntry, wordSubmissionMaxEntries)
	for i := range english {
		english[i] = englishSubmissionEntry{strings.Repeat("a", 64), strings.Repeat("A", 63) + string(rune('A'+i))}
		translations[i] = translationSubmissionEntry{strings.Repeat("好", 64), strings.Repeat("x", 199) + string(rune('a'+i))}
	}
	for _, tc := range []struct {
		name string
		run  func() []wordRejection
	}{
		{"english/previous", func() []wordRejection { return previousValidateEnglishEntries(english) }},
		{"english/current", func() []wordRejection { return validateEnglishEntries(english) }},
		{"translations/previous", func() []wordRejection { return previousValidateTranslationEntries(translations) }},
		{"translations/current", func() []wordRejection { return validateTranslationEntries(translations) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			if rejected := tc.run(); len(rejected) != 0 {
				b.Fatalf("有效词条被拒绝：%#v", rejected)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = tc.run()
			}
		})
	}
}
