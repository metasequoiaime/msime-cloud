package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

var (
	previousQQPinyinPunctuation = strings.NewReplacer("（", " ", "）", " ", "(", " ", ")", " ", "[", " ", "]", " ", "【", " ", "】", " ", "「", " ", "」", " ", "《", " ", "》", " ", "“", " ", "”", " ", "\"", " ")
	previousQQPinyinCleanup     = strings.NewReplacer("u:", "v", "’", "'", "-", " ", "　", " ")
)

// previousQQNormalizePinyinStages 保留普通拼音分支及缓存后的分阶段替换，作为当前实现的兼容性和性能基准。
func previousQQNormalizePinyinStages(raw string) string {
	if strings.IndexFunc(raw, func(r rune) bool {
		return (r < 'a' || r > 'z') && r != ' ' && r != '\''
	}) < 0 {
		var out strings.Builder
		for part := range strings.FieldsFuncSeq(raw, func(r rune) bool { return r == ' ' || r == '\'' }) {
			if out.Len() == 0 {
				out.Grow(len(raw))
			} else {
				out.WriteByte('\'')
			}
			out.WriteString(part)
		}
		return out.String()
	}
	raw = strings.Map(func(r rune) rune {
		if plain, ok := qqToneless[r]; ok {
			return plain
		}
		return r
	}, strings.ToLower(raw))
	raw = previousQQPinyinPunctuation.Replace(raw)
	raw = previousQQPinyinCleanup.Replace(raw)
	return strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '\'' }), "'")
}

func checkQQPinyinStages(t testing.TB, input string) {
	t.Helper()
	if got, want := qqNormalizePinyin(input), previousQQNormalizePinyinStages(input); got != want {
		t.Fatalf("拼音转换顺序变化：输入=%q，结果=%q，期望=%q", input, got, want)
	}
}

func TestQQPinyinStagesCompatibility(t *testing.T) {
	for _, input := range []string{
		"", "shui shan", "SHUI SHAN", "（LǛ-SÈ）", "u:", "U:", "ū:", "Ū:", "ü:", "Ü:", "Ǜ:",
		"u:u::", "u（:）", "u’:", "u-:", "u'（:）", "u\t:", "u\x00:", "U\xff:", "\xffU:\xe4\xb8",
		"（’）-　", "\t（LǛ）\n", "İ K Σ ẞ", "u\u0085:\u00a0U:\u2003", "‘u:’", "u：", "ｕ:", "（u）:",
	} {
		checkQQPinyinStages(t, input)
	}
	for _, left := range []string{"u", "U", "ū", "Ū", "ü", "Ü", "Ǜ", "ǜ", "u:", "\xffu"} {
		for _, middle := range []string{"", " ", "'", "’", "-", "　", "（", "）", "(", ")", "[", "]", "【", "】", "「", "」", "《", "》", "“", "”", "\"", "\t", "\r", "\n", "\xff"} {
			checkQQPinyinStages(t, left+middle+":")
		}
	}
	random := rand.New(rand.NewSource(1058))
	units := []string{"a", "U:", "u:", "ū", "Ū", "Ü", "Ǜ", "ā", "é", "ǐ", "ò", "u", ":", "'", "’", "-", " ", "　", "\t", "\n", "\r", "\u0085", "\u00a0", "（", "）", "“", "”", "[", "]", "水", "İ", "K", "Σ", "\xff", "\xe4\xb8"}
	for range 5000 {
		var input strings.Builder
		for range random.Intn(60) {
			input.WriteString(units[random.Intn(len(units))])
		}
		checkQQPinyinStages(t, input.String())
	}
}

func FuzzQQPinyinStages(f *testing.F) {
	for _, input := range []string{"", "（LǛ-SÈ）", "Ū:u（:）u’:", "\xffU:\xe4\xb8", "\t（LǛ）\n", "İ K Σ", "（’）-　"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		checkQQPinyinStages(t, input)
	})
}

func TestQQPinyinStagesAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		budget      int64
	}{
		{"short_tonal", "（LǛ-SÈ）", 64},
		{"long_tonal", strings.Repeat("LǛ- ", 300), 2048},
		{"uppercase", strings.Repeat("NI HAO ", 250), 2304},
		{"separators", strings.Repeat("（’）-", 300), 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if utf8.RuneCountInString(tc.input) > qqBotMaxRunes {
				t.Fatal("回归输入超过实际消息长度上限")
			}
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = qqNormalizePinyin(tc.input)
				}
			})
			if want := previousQQNormalizePinyinStages(tc.input); got != want {
				t.Fatalf("拼音规范化结果=%q，期望=%q", got, want)
			}
			if result.AllocedBytesPerOp() > tc.budget {
				t.Fatalf("规范化分配 %s，超过 %d 字节预算", result.MemString(), tc.budget)
			}
		})
	}
}

func BenchmarkQQPinyinStages(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", "shui shan shu ru fa"},
		{"tonal", "（LǛ-SÈ）"},
		{"uppercase", "LU: SE"},
		{"long_tonal", strings.Repeat("LǛ- ", 300)},
		{"long_uppercase", strings.Repeat("NI HAO ", 250)},
		{"separators", strings.Repeat("（’）-", 300)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) string
		}{{"previous", previousQQNormalizePinyinStages}, {"current", qqNormalizePinyin}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input)
				}
			})
		}
	}
}
