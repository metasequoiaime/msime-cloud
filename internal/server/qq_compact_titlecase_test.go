package server

import (
	"strings"
	"testing"
	"unicode"
)

func TestQQCompactTitlecaseNormalizesBeforeReuse(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"ǅ", "ǆ"},
		{"ǅ䷿", "ǆ"},
		{"ǈ🙏", "ǉ"},
		{"ǋ？", "ǌ"},
		{"ǲ", "ǳ"},
		{"水ǅ杉", "水ǆ杉"},
		{"ᾈ", "ᾀ"},
	} {
		if got := qqCompact(tc.input); got != tc.want {
			t.Errorf("标题字母未转成小写：输入=%q，结果=%q，期望=%q", tc.input, got, tc.want)
		}
	}
}

func TestQQCompactTitlecaseUnicodeCategory(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.IsTitle(r) {
			continue
		}
		input := string(r)
		want := strings.ToLower(input)
		if qqCompactCanReuse(input) {
			t.Fatalf("标题字母被判为可原样复用：%U", r)
		}
		for _, suffix := range []string{"", "？", "🙏", "䷿"} {
			if got := qqCompact(input + suffix); got != want {
				t.Fatalf("标题字母规范化错误：输入=%q，结果=%q，期望=%q", input+suffix, got, want)
			}
		}
	}
}
