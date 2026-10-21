package server

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var previousQQWordSeparators = regexp.MustCompile(`[,，;；、\n]+`)

// previousQQParseWords 保留先拆分全部片段再解析的流程，用于兼容性和性能比较。
func previousQQParseWords(content string) (entries []wordSubmissionEntry, lettered bool) {
	for _, part := range previousQQWordSeparators.Split(content, -1) {
		part = strings.TrimSpace(strings.Trim(strings.TrimSpace(part), qqWordQuotes))
		if part == "" {
			continue
		}
		word, pinyin := part, ""
		if i := strings.IndexFunc(part, func(r rune) bool { return !wordCharacter(r) }); i > 0 {
			word, pinyin = part[:i], qqNormalizePinyin(part[i:])
			pinyin = qqSplitPinyin(pinyin, utf8.RuneCountInString(word))
			if c := part[i]; ('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') && len(validateWordEntries([]wordSubmissionEntry{{Word: word, Pinyin: pinyin}})) > 0 {
				lettered = true
			}
		}
		entries = append(entries, wordSubmissionEntry{Word: word, Pinyin: pinyin})
	}
	return entries, lettered
}

func previousQQWordsQuestion(content string, glued bool) bool {
	entries, _ := previousQQParseWords(content)
	for _, e := range entries {
		if qqExplicitPinyin.MatchString(e.Pinyin) {
			return false
		}
	}
	return qqCommandAsks(content, glued) || qqContainsQuestion(content)
}

func checkQQWordStream(t testing.TB, input string) {
	t.Helper()
	got, lettered := qqParseWords(input)
	want, wantLettered := previousQQParseWords(input)
	if !reflect.DeepEqual(got, want) || lettered != wantLettered {
		t.Fatalf("词条解析变化：输入=%q，结果=%+v/%v，期望=%+v/%v", input, got, lettered, want, wantLettered)
	}
	for _, glued := range []bool{false, true} {
		if got, want := qqWordsQuestion(input, glued), previousQQWordsQuestion(input, glued); got != want {
			t.Fatalf("加词提问判断变化：输入=%q，紧接=%v，结果=%v，期望=%v", input, glued, got, want)
		}
	}
}

func TestQQWordStreamCompatibility(t *testing.T) {
	for _, input := range []string{
		"", ",，;；、\n", " \t\r\u3000", "「」", "水杉", "水杉 shui shan", "水杉shuishan",
		"水杉、未来，绿色;女儿；西安\n输入法", "栓Q shuan q", "栓Q，水杉 shui shan", "水杉 shui shan，栓Q",
		"怎么用，酒吧 jiu ba", "怎么用，水杉", "水杉，怎么用", "酒吧 jiu ba，怎么用", "怎么用，，，", ",,怎么用",
		"《水杉》 shui shan", "“绿色” （LǛ-SÈ）", "女儿 nu:’er", "\xff，\xe4\xb8;水杉", "水杉\r\n未来",
		"水杉\v未来\t怎么用", "水杉\u0085未来\u2028怎么用", "水杉 shui shan，" + strings.Repeat("词语，", 600),
	} {
		checkQQWordStream(t, input)
	}
	random := rand.New(rand.NewSource(1057))
	units := []string{"水", "杉", "词语", "怎么", "吗", "吧", "a", "shui", "Q", "LǛ", "u:", "'", "’", "（", "）", "「", "」", ",", "，", ";", "；", "、", "\n", "\r", "\t", " ", "\u3000", "\xff", "\xe4\xb8"}
	for range 2000 {
		var input strings.Builder
		for range random.Intn(40) {
			input.WriteString(units[random.Intn(len(units))])
		}
		checkQQWordStream(t, input.String())
	}
}

func FuzzQQWordStream(f *testing.F) {
	for _, input := range []string{"", "水杉 shui shan，栓Q", "怎么用，酒吧 jiu ba", "《绿色》 （LǛ-SÈ）", "水杉\xff，词语", ",，;；、\n"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		checkQQWordStream(t, input)
	})
}

func TestQQWordsQuestionAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"first_pinyin", "水杉 shui shan，" + strings.Repeat("词语，", 600), false},
		{"no_pinyin", strings.Repeat("词语，", 600) + "怎么用", true},
		{"separators", strings.Repeat("，;", 900), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if utf8.RuneCountInString(tc.input) > qqBotMaxRunes {
				t.Fatal("回归输入超过实际消息长度上限")
			}
			var got bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqWordsQuestion(tc.input, false)
				}
			})
			if got != tc.want {
				t.Fatalf("加词提问判断结果=%v，期望=%v", got, tc.want)
			}
			if result.AllocedBytesPerOp() > 1024 {
				t.Fatalf("提问判断分配 %s，超过 1 KiB 预算", result.MemString())
			}
		})
	}
}

func TestQQParseWordsManyEntriesAllocationBudget(t *testing.T) {
	input := strings.Repeat("词语，", 600)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			entries, _ := qqParseWords(input)
			if len(entries) != 600 {
				b.Fatalf("解析条数=%d，期望 600", len(entries))
			}
		}
	})
	if result.AllocedBytesPerOp() >= 32<<10 {
		t.Fatalf("600 条词语解析分配 %d B，超过 32 KiB 预算", result.AllocedBytesPerOp())
	}
}

func BenchmarkQQWordsQuestionStream(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"typical", "水杉输入法 shui shan shu ru fa"},
		{"first_pinyin", "水杉 shui shan，" + strings.Repeat("词语，", 600)},
		{"no_pinyin", strings.Repeat("词语，", 600) + "怎么用"},
		{"separators", strings.Repeat("，;", 900)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string, bool) bool
		}{{"previous", previousQQWordsQuestion}, {"current", qqWordsQuestion}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input, false)
				}
			})
		}
	}
}

func BenchmarkQQParseWordsStream(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"typical", "水杉输入法 shui shan shu ru fa"},
		{"mixed", "水杉 shui shan，绿色（LǛ-SÈ），女儿 nu:’er"},
		{"many_entries", strings.Repeat("词语，", 600)},
		{"separators", strings.Repeat("，;", 900)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) ([]wordSubmissionEntry, bool)
		}{{"previous", previousQQParseWords}, {"current", qqParseWords}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = impl.run(tc.input)
				}
			})
		}
	}
}
