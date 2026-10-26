package server

import (
	"math/rand"
	"strings"
	"testing"
)

func previousQQNormalizePinyin(raw string) string {
	raw = strings.Map(func(r rune) rune {
		if plain, ok := qqToneless[r]; ok {
			return plain
		}
		return r
	}, strings.ToLower(raw))
	raw = previousQQPinyinPunctuation.Replace(raw)
	raw = strings.NewReplacer("u:", "v", "’", "'", "-", " ", "　", " ").Replace(raw)
	return strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '\'' }), "'")
}

func TestQQNormalizePinyinMatchesPrevious(t *testing.T) {
	inputs := []string{"", "shui'shan", "SHUI SHAN", "（LǛ-SÈ）", "lu:　se", "u:u:", "nu:’er", "u（:）", "' ' -　", "\tu:\n", "\xffu:\xc0", "İ K Σ", "水杉"}
	random := rand.New(rand.NewSource(59))
	units := []string{"a", "U:", "u:", "Ü", "Ǜ", "ā", "é", "ǐ", "ò", "ū", "u", ":", "'", "’", "-", " ", "　", "\t", "\n", "\u0085", "（", "）", "“", "”", "[", "]", "水", "\xff", "\xe4\xb8"}
	for range 500 {
		var input strings.Builder
		for range random.Intn(50) {
			input.WriteString(units[random.Intn(len(units))])
		}
		inputs = append(inputs, input.String())
	}
	for _, input := range inputs {
		if got, want := qqNormalizePinyin(input), previousQQNormalizePinyin(input); got != want {
			t.Fatalf("拼音规范化不兼容：输入=%q，结果=%q，期望=%q", input, got, want)
		}
	}
}

func TestQQNormalizePinyinConcurrent(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"普通拼音", "shui shan shu ru fa", "shui'shan'shu'ru'fa"},
		{"大写拼音", "LU: SE", "lv'se"},
		{"带声调括号", "（LǛ-SÈ）", "lv'se"},
		{"弯引号", "nu:’er", "nv'er"},
		{"全角空格", "lu:　se", "lv'se"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for range 100 {
				if got := qqNormalizePinyin(tc.input); got != tc.want {
					t.Fatalf("并发规范化结果=%q，期望=%q", got, tc.want)
				}
			}
		})
	}
}

func TestQQNormalizePinyinReusesRules(t *testing.T) {
	for _, input := range []string{"shui shan shu ru fa", "（LǛ-SÈ）", "lu:　se"} {
		var got string
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got = qqNormalizePinyin(input)
			}
		})
		if got != previousQQNormalizePinyin(input) {
			t.Fatal("拼音规范化结果发生变化")
		}
		if allocated := result.AllocedBytesPerOp(); allocated > 512 {
			t.Fatalf("规范化短拼音分配 %d 字节，超过 512 字节预算", allocated)
		}
	}
}

func TestQQNormalizePinyinPlainAllocationBudget(t *testing.T) {
	const input = "shui shan shu ru fa"
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqNormalizePinyin(input)
		}
	})
	if got != "shui'shan'shu'ru'fa" {
		t.Fatalf("普通拼音规范化结果发生变化：%q", got)
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 64 {
		t.Fatalf("普通拼音规范化分配 %d 字节，超过 64 字节预算", allocated)
	}
}

func TestQQNormalizePinyinPlainMatchesPrevious(t *testing.T) {
	inputs := []string{"", "   ", "'''", "a", "shui'shan", "  shui  ' shan '' ", "shui\tshan", "shui\nshan", "SHUI SHAN", "shui-shan"}
	random := rand.New(rand.NewSource(61))
	const alphabet = "abcdefghijklmnopqrstuvwxyz '"
	for range 500 {
		var input strings.Builder
		for range random.Intn(200) {
			input.WriteByte(alphabet[random.Intn(len(alphabet))])
		}
		inputs = append(inputs, input.String())
	}
	for _, input := range inputs {
		if got, want := qqNormalizePinyin(input), previousQQNormalizePinyin(input); got != want {
			t.Fatalf("普通拼音规范化不兼容：输入=%q，结果=%q，期望=%q", input, got, want)
		}
	}
}

func BenchmarkQQPinyinNormalize(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", "shui shan shu ru fa"},
		{"tonal", "（LǛ-SÈ）"},
		{"variants", "lu:　se"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqNormalizePinyin(tc.input)
			}
		})
	}
}

func BenchmarkQQParseWordsPinyin(b *testing.B) {
	input := "水杉输入法 shui shan shu ru fa，绿色（LǛ-SÈ），女儿 nu:’er"
	b.ReportAllocs()
	for b.Loop() {
		_, _ = qqParseWords(input)
	}
}
