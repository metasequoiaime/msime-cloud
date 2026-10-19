package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func referenceQQCompactCanReuse(text string) bool {
	if text == "" || strings.HasPrefix(text, "/") {
		return false
	}
	if strings.IndexFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsUpper(r) || unicode.IsTitle(r) || r >= 0xFF01 && r <= 0xFF5E
	}) >= 0 {
		return false
	}
	last, size := utf8.DecodeLastRuneInString(text)
	return size == len(text) || (!strings.ContainsRune(qqCommandTrailing, last) && !unicode.In(last, unicode.So, unicode.Sk) && last != 0xFE0F && last != 0x200D)
}

func TestQQCompactHanUnicodeProperties(t *testing.T) {
	for r := rune(0x4E00); r <= 0x9FFF; r++ {
		if unicode.IsSpace(r) || unicode.IsUpper(r) || unicode.ToLower(r) != r {
			t.Fatalf("汉字区间出现需要规范化的字符：%U", r)
		}
		if input := string(r); !qqCompactCanReuse(input) || qqCompact(input) != input {
			t.Fatalf("单个汉字不能原样复用：%U", r)
		}
	}
}

func TestQQCompactHanReuseMatchesUnicodeChecks(t *testing.T) {
	units := []string{"", "水", "杉", "\u4dff", "\u4e00", "\u9fff", "\ua000", "\U00020000", "a", "A", "é", "É", "Δ", "ǅ", "ᾈ", "/", "／", "?", "？", "🙏", "\ufe0f", "\u200d", " ", "\u3000", "\u0085", "\xff", "\xe4\xb8"}
	check := func(input string) {
		t.Helper()
		if got, want := qqCompactCanReuse(input), referenceQQCompactCanReuse(input); got != want {
			t.Fatalf("复用判定变化：输入=%q，结果=%t，期望=%t", input, got, want)
		}
	}
	for r := rune(0); r <= unicode.MaxLatin1; r++ {
		check(string(r))
		check("水" + string(r))
		check(string(r) + "水")
	}
	for _, first := range units {
		for _, last := range units {
			check(first + "水" + last)
			check(first + last)
		}
	}
	random := rand.New(rand.NewSource(951))
	for range 2000 {
		var input strings.Builder
		for range random.Intn(80) {
			input.WriteString(units[random.Intn(len(units))])
		}
		check(input.String())
	}
}

func qqCompactHanCases() []struct{ name, input string } {
	body := strings.Repeat("候选框在哪里", 8192)
	return []struct{ name, input string }{
		{"短中文", "候选框在哪里"},
		{"长中文", body},
		{"中文后标点", body + "？"},
		{"中文后表情", body + "🙏"},
		{"中文后大写", body + "HELP"},
		{"中文后空白", body + " "},
		{"中文与ASCII混排", strings.Repeat("候选框 abc123", 8192)},
		{"小写ASCII", strings.Repeat("help123", 8192)},
		{"大写ASCII", strings.Repeat("HELP123", 8192)},
		{"Latin1", strings.Repeat("éñ", 8192)},
		{"希腊文", strings.Repeat("αβγ", 8192)},
		{"扩展汉字", strings.Repeat("\U00020000", 8192)},
	}
}

func BenchmarkQQCompactHan(b *testing.B) {
	for _, tc := range qqCompactHanCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompact(tc.input)
			}
		})
	}
}
