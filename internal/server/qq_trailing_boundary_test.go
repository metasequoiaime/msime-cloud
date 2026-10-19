package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestQQTrimCommandTrailingByteBoundaries(t *testing.T) {
	check := func(input string) {
		t.Helper()
		want := input
		// 旧算法统计全文字符数，与首字符字节边界独立比较。
		for count := utf8.RuneCountInString(want); count > 1; count-- {
			r, size := utf8.DecodeLastRuneInString(want)
			if !strings.ContainsRune(qqCommandTrailing, r) && !unicode.In(r, unicode.So, unicode.Sk) && r != 0xFE0F && r != 0x200D {
				break
			}
			want = want[:len(want)-size]
		}
		if got := qqTrimCommandTrailing(input); got != want {
			t.Fatalf("尾部裁剪的字符边界变化：输入=%q，结果=%q，期望=%q", input, got, want)
		}
	}
	check("")
	for first := range 256 {
		check(string([]byte{byte(first)}))
		for last := range 256 {
			check(string([]byte{byte(first), byte(last)}))
		}
	}
	units := []string{"a", "水", "é", "𠀀", "?", "。", "🙏", "🏽", "\ufe0f", "\u200d", "\ufffd", "\xff", "\xe4\xb8"}
	random := rand.New(rand.NewSource(967))
	for range 2000 {
		var input strings.Builder
		for range random.Intn(100) {
			input.WriteString(units[random.Intn(len(units))])
		}
		check(input.String())
	}
}

func BenchmarkQQTrimCommandTrailingBoundary(b *testing.B) {
	body := strings.Repeat("候选框在哪里", 333)
	for _, tc := range []struct{ name, input string }{
		{"短命令", "帮助"},
		{"单表情", "🙏"},
		{"长中文", body},
		{"长ASCII", strings.Repeat("hello", 400)},
		{"长正文短尾部", body + "🙏"},
		{"长正文长尾部", body + strings.Repeat("🙏", 1024)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqTrimCommandTrailing(tc.input)
			}
		})
	}
}
