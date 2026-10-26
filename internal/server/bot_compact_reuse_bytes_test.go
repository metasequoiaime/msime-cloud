package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQQCompactReuseByteBoundaries(t *testing.T) {
	check := func(middle string) {
		t.Helper()
		for _, input := range []string{middle, "水" + middle + "杉", "a" + middle + "b", "水" + middle + "a杉"} {
			want := referenceQQCompactCanReuse(input)
			for _, valid := range []bool{false, true} {
				if got := qqCompactReusable(input, valid); got != (want && (!valid || utf8.ValidString(input))) {
					t.Fatalf("复用字节边界变化：输入=%q，要求有效UTF8=%t，结果=%t", input, valid, got)
				}
			}
			if got, want := qqCompact(input), referenceQQCompactSpans(input); got != want {
				t.Fatalf("字节边界规范化变化：输入=%q，结果=%q，期望=%q", input, got, want)
			}
		}
	}
	for r := rune(0x4DFF); r <= 0xA000; r++ {
		check(string(r))
	}
	for value := range 256 {
		check(string([]byte{byte(value)}))
		for _, han := range [][3]byte{{0xE4, 0xB8, 0x80}, {0xE9, 0xBF, 0xBF}} {
			for offset := range han {
				bytes := han
				bytes[offset] = byte(value)
				check(string(bytes[:]))
			}
		}
	}
	for _, middle := range []string{"\xe4", "\xe4\xb8", "\xe9", "\xe9\xbf", "\ufffd", "\xef\xbf", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\U00020000", "\u200b", "ǅ", "é", "α"} {
		check(middle)
	}
}

func qqCompactReuseByteCases() []struct{ name, input string } {
	body := strings.Repeat("候选框在哪里", 333)
	return []struct{ name, input string }{
		{"短中文", "候选框在哪里"},
		{"短ASCII", "help123"},
		{"两千字中文", body},
		{"中文后大写", body + "HELP"},
		{"中文与ASCII", strings.Repeat("候选框abc123", 200)},
		{"ASCII长正文", strings.Repeat("help123", 1000)},
		{"Latin1", strings.Repeat("éñ", 1000)},
		{"希腊文", strings.Repeat("αβγ", 667)},
		{"扩展汉字", strings.Repeat("\U00020000", 2000)},
		{"无效UTF8", "水\xff" + body + "杉"},
	}
}

func BenchmarkQQCompactReuseBytes(b *testing.B) {
	for _, tc := range qqCompactReuseByteCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompactReusable(tc.input, true)
			}
		})
	}
}
