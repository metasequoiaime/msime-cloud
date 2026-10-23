package server

import (
	"strings"
	"testing"
)

func TestQQCompactTailFirstReuseMatchesPrevious(t *testing.T) {
	check := func(input string) {
		t.Helper()
		if got, want := qqCompactCanReuse(input), referenceQQCompactCanReuse(input); got != want {
			t.Fatalf("尾部复用判定变化：输入=%q，结果=%t，期望=%t", input, got, want)
		}
	}
	for _, prefix := range []string{"", "水", "A", "/", "水\xff", "\u3000水"} {
		for _, suffix := range []string{"", "?", "!", "。", ".", "~", "…", "？", "！", "Ａ", "🙏", "🏽", "\ufe0f", "\u200d", "\ufffd", "水", "ǅ", "é", "\u0301", " ", "\u0085", "\u3000", "\xff", "\xe4\xb8"} {
			check(prefix + suffix)
		}
		for r := rune(0xFF01); r <= 0xFF5E; r++ {
			check(prefix + string(r))
		}
	}
	// 穷举单字节和双字节，覆盖反向解码与正向遍历在无效 UTF-8 上的边界。
	for first := range 256 {
		one := string([]byte{byte(first)})
		check(one)
		check("水" + one)
		for second := range 256 {
			two := string([]byte{byte(first), byte(second)})
			check(two)
			check("水" + two)
		}
	}
}

func TestQQCompactTailFirstKeepsSingleRune(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"?", "?"},
		{"？", "?"},
		{"🙏", "🙏"},
		{"\ufe0f", "\ufe0f"},
		{"\u200d", "\u200d"},
		{"\xff", "\xff"},
		{"\ufffd", "\ufffd"},
		{"\xe4\xb8", "\ufffd"},
		{"水?", "水"},
		{"水？", "水"},
		{"水🙏", "水"},
		{"水\ufe0f", "水"},
		{"水\u200d", "水"},
		{"水\xff", "水"},
		{"水\ufffd", "水"},
	} {
		if got := qqCompact(tc.input); got != tc.want {
			t.Errorf("单字符或尾部裁剪变化：输入=%q，结果=%q，期望=%q", tc.input, got, tc.want)
		}
	}
}

func BenchmarkQQCompactTailFirstReuse(b *testing.B) {
	body := strings.Repeat("候选框在哪里", 333)
	for _, tc := range []struct{ name, input string }{
		{"短中文", "帮助"},
		{"短ASCII大写", "Help me"},
		{"单个问号", "?"},
		{"短中文问号", "帮助？"},
		{"长中文", body},
		{"长中文半角问号", body + "?"},
		{"长中文全角问号", body + "？"},
		{"长中文表情", body + "🙏"},
		{"长中文变体选择符", body + "\ufe0f"},
		{"长中文无效UTF8", body + "\xff"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompactCanReuse(tc.input)
			}
		})
	}
}
