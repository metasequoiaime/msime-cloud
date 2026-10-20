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

// previousValidateWordEntries 保留切片版本，校验错误优先级、原因和分配变化。
func previousValidateWordEntries(entries []wordSubmissionEntry) []wordRejection {
	rejected := []wordRejection{}
	seen := map[string]bool{}
	for i, e := range entries {
		reject := func(code, reason string) { rejected = append(rejected, wordRejection{i, code, reason}) }
		characters := utf8.RuneCountInString(e.Word)
		switch {
		case e.Word == "":
			reject("word_required", "请填写词语")
			continue
		case !utf8.ValidString(e.Word) || strings.IndexFunc(e.Word, func(r rune) bool { return !wordCharacter(r) }) >= 0:
			reject("invalid_word", "词语只能包含汉字，不能有字母、数字、标点或空格")
			continue
		case characters > wordSubmissionMaxChars:
			reject("word_too_long", "词语最多 16 个汉字")
			continue
		}
		if e.Pinyin == "" {
			reject("pinyin_required", "请填写拼音")
			continue
		}
		syllables := strings.Split(e.Pinyin, "'")
		if len(e.Pinyin) > 200 || slices.ContainsFunc(syllables, func(s string) bool {
			return s == "" || strings.IndexFunc(s, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0
		}) {
			reject("invalid_pinyin", "拼音只能包含小写字母，音节之间用 ' 分隔，例如 wei'lai'ke'qi")
			continue
		}
		if bad := slices.IndexFunc(syllables, func(s string) bool { return !pinyinSyllables[s] }); bad >= 0 {
			reject("invalid_syllable", "“"+syllables[bad]+"”不是有效的全拼音节（ü 请写作 v，如 lv、nve）")
			continue
		}
		if len(syllables) != characters {
			reject("syllable_count_mismatch", "“"+e.Word+"”有 "+strconv.Itoa(characters)+" 个字，但拼音有 "+strconv.Itoa(len(syllables))+" 个音节")
			continue
		}
		key := e.Word + "\t" + e.Pinyin
		if seen[key] {
			reject("duplicate_entry", "“"+e.Word+"”重复填写了")
			continue
		}
		seen[key] = true
	}
	return rejected
}

func checkWordPinyinValidation(t testing.TB, entries []wordSubmissionEntry) {
	t.Helper()
	if got, want := validateWordEntries(entries), previousValidateWordEntries(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("词条验证结果变化：输入=%q，结果=%#v，期望=%#v", entries, got, want)
	}
}

func TestWordPinyinValidationCompatibility(t *testing.T) {
	for _, tc := range []struct{ pinyin, code string }{
		{"ni'hao", ""}, {"", "pinyin_required"}, {"lue'hao", "invalid_syllable"},
		{"lue''hao", "invalid_pinyin"}, {"lue'Ni", "invalid_pinyin"}, {"lue'hao'", "invalid_pinyin"},
		{"lue'\xff", "invalid_pinyin"}, {"lue'nǐ", "invalid_pinyin"}, {"lue'hoa", "invalid_syllable"},
		{"ni", "syllable_count_mismatch"}, {strings.Repeat("a", 200), "invalid_syllable"},
		{strings.Repeat("a", 201), "invalid_pinyin"}, {strings.Repeat("'", 12000), "invalid_pinyin"},
	} {
		entries := []wordSubmissionEntry{{Word: "你好", Pinyin: tc.pinyin}}
		checkWordPinyinValidation(t, entries)
		got := validateWordEntries(entries)
		if tc.code == "" {
			if len(got) != 0 {
				t.Fatalf("有效词条被拒绝：%#v", got)
			}
		} else if len(got) != 1 || got[0].Code != tc.code {
			t.Fatalf("拼音=%q，拒绝结果=%#v，期望错误码=%s", tc.pinyin, got, tc.code)
		}
	}
	checkWordPinyinValidation(t, nil)
	checkWordPinyinValidation(t, []wordSubmissionEntry{{"绿", "lv"}, {"绿", "lv"}, {"绿", "lue''"}, {"绿", "lv"}})
	for _, syllable := range strings.Fields(pinyinSyllableTable) {
		checkWordPinyinValidation(t, []wordSubmissionEntry{{"好", syllable}, {"好", syllable}})
	}
	random := rand.New(rand.NewSource(1059))
	units := []string{"ni", "hao", "lv", "lve", "nve", "yi", "er", "a", "lue", "hoa", "'", " ", "N", "ǐ", "\xff", "\x00"}
	words := []string{"", "你好", "绿", "略", "〇", "abc", "你 好", "\xff", strings.Repeat("好", 16), strings.Repeat("好", 17)}
	for range 5000 {
		var pinyin strings.Builder
		for range random.Intn(100) {
			pinyin.WriteString(units[random.Intn(len(units))])
		}
		word := words[random.Intn(len(words))]
		entries := []wordSubmissionEntry{{word, pinyin.String()}, {"你好", "ni'hao"}, {word, pinyin.String()}, {"你好", "ni'hao"}}
		checkWordPinyinValidation(t, entries)
	}
}

func FuzzWordPinyinValidation(f *testing.F) {
	for _, pinyin := range []string{"ni'hao", "lue'Ni", "lue''hao", "lue'hoa", "\xff", strings.Repeat("'", 12000)} {
		f.Add("你好", pinyin)
	}
	f.Fuzz(func(t *testing.T, word, pinyin string) {
		checkWordPinyinValidation(t, []wordSubmissionEntry{{word, pinyin}, {"你好", "ni'hao"}, {word, pinyin}, {"你好", "ni'hao"}})
	})
}

func TestWordPinyinValidationAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, word, pinyin string
		budget             int64
	}{
		{"oversized_separators", "你好", strings.Repeat("'", 12000), 128},
		{"sixteen_syllables", strings.Repeat("好", 16), strings.TrimSuffix(strings.Repeat("hao'", 16), "'"), 192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.word)+len(tc.pinyin)+100 > wordSubmissionBodyBytes {
				t.Fatal("回归输入超过投稿请求大小限制")
			}
			entries := []wordSubmissionEntry{{tc.word, tc.pinyin}}
			var got []wordRejection
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = validateWordEntries(entries)
				}
			})
			if want := previousValidateWordEntries(entries); !reflect.DeepEqual(got, want) {
				t.Fatalf("词条验证结果=%#v，期望=%#v", got, want)
			}
			if result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("验证分配 %s，超过 %d 字节预算", result.MemString(), tc.budget)
			}
		})
	}
}

func BenchmarkWordPinyinValidation(b *testing.B) {
	batch := make([]wordSubmissionEntry, wordSubmissionMaxEntries)
	for i := range batch {
		batch[i] = wordSubmissionEntry{string(rune('一'+i)) + strings.Repeat("好", 15), strings.TrimSuffix(strings.Repeat("hao'", 16), "'")}
	}
	for _, tc := range []struct {
		name    string
		entries []wordSubmissionEntry
	}{
		{"short", []wordSubmissionEntry{{"你好", "ni'hao"}}},
		{"sixteen_syllables", batch[:1]},
		{"twenty_entries", batch},
		{"oversized_separators", []wordSubmissionEntry{{"你好", strings.Repeat("'", 12000)}}},
		{"late_syntax_error", []wordSubmissionEntry{{"你好", "lue'" + strings.Repeat("hao'", 40)}}},
	} {
		for _, impl := range []struct {
			name string
			run  func([]wordSubmissionEntry) []wordRejection
		}{{"previous", previousValidateWordEntries}, {"current", validateWordEntries}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.entries)
				}
			})
		}
	}
}
