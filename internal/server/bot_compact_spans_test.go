package server

import (
	"math/rand"
	"strings"
	"testing"
)

func referenceQQCompactSpans(text string) string {
	if referenceQQCompactCanReuse(text) {
		return text
	}
	compact := strings.TrimPrefix(strings.ToLower(strings.Join(strings.Fields(qqFoldWidth(text)), "")), "/")
	return qqTrimCommandTrailing(compact)
}

func TestQQCompactSpansMatchesReference(t *testing.T) {
	check := func(input string) {
		t.Helper()
		if got, want := qqCompact(input), referenceQQCompactSpans(input); got != want {
			t.Fatalf("片段规范化不兼容：输入=%q，结果=%q，期望=%q", input, got, want)
		}
	}
	for r := rune(0); r <= 0xFF; r++ {
		check(" / 水" + string(r) + "杉？")
		check(string(r) + " ")
	}
	for r := rune(0x4E00); r <= 0x9FFF; r++ {
		check(" / " + string(r) + " 水Ａ？")
	}
	for r := rune(0xFF01); r <= 0xFF5E; r++ {
		check(" / 水" + string(r) + "杉？")
	}
	units := []string{"", "水", "杉", "\u4dff", "\u9fff", "\ua000", "\U00020000", "a", "A", "É", "é", "Ⱥ", "K", "İ", "ẞ", "ǅ", "Δ", "α", "\u0301", "／", "/", "?", "？", "🙏", "\ufe0f", "\u200d", "\ufffd", " ", "\t", "\n", "\u0085", "\u00a0", "\u2002", "\u3000", "\xff", "\xe4\xb8", "\xed\xa0\x80", "\xc0\xaf"}
	for _, first := range units {
		for _, middle := range units {
			for _, last := range units {
				check(first + middle + last)
			}
		}
	}
	random := rand.New(rand.NewSource(953))
	for range 3000 {
		var input strings.Builder
		input.WriteString(" / ")
		for range random.Intn(80) {
			if random.Intn(3) == 0 {
				input.WriteRune(rune(random.Intn(0x110000)))
			} else {
				input.WriteString(units[random.Intn(len(units))])
			}
		}
		check(input.String())
	}
	for _, tc := range qqCompactSpanCases() {
		check(tc.input)
	}
}

func TestQQCompactHanContinuationMatchesReference(t *testing.T) {
	check := func(middle string) {
		t.Helper()
		for _, input := range []string{" / 水" + middle + "杉Ａ？", " / 水" + middle} {
			if got, want := qqCompact(input), referenceQQCompactSpans(input); got != want {
				t.Fatalf("汉字片段边界不兼容：输入=%q，结果=%q，期望=%q", input, got, want)
			}
		}
	}
	for r := rune(0x4DFF); r <= 0xA000; r++ {
		check(string(r))
	}
	// 分别遍历三个字节位置，验证无效序列不能作为基本汉字原样复制。
	for value := range 256 {
		check(string([]byte{byte(value), 0xB8, 0x80}))
		check(string([]byte{0xE4, byte(value), 0x80}))
		check(string([]byte{0xE4, 0xB8, byte(value)}))
	}
	for _, middle := range []string{"\xe4", "\xe4\xb8", "\xe9", "\xe9\xbf", "\U00020000", "\ufffd", "\ud7ff", "\ue000"} {
		check(middle)
	}
}

func qqCompactSpanCases() []struct{ name, input string } {
	body := strings.Repeat("候选框在哪里", 8192)
	return []struct{ name, input string }{
		{"短命令", "/Help？"},
		{"短中文空白", "候选 框 在哪 里"},
		{"两千字全角正文", "ＨＥＬＰ　" + strings.Repeat("候选框在哪里", 333)},
		{"常见长度大小写混排", strings.Repeat("候选框 ABC123 ", 100)},
		{"中文后大写", body + "HELP"},
		{"中文后空白", body + " "},
		{"中文与ASCII混排", strings.Repeat("候选框 abc123", 8192)},
		{"中文密集空白", strings.Repeat("候 选 框 在 哪 里 ", 8192)},
		{"中文后全角", body + "Ｈｅｌｐ"},
		{"前缀命令长中文", "/" + body},
		{"ASCII空白", strings.Repeat("help123 ", 8192)},
		{"ASCII大写", strings.Repeat("HELP123", 8192)},
		{"Latin1空白", strings.Repeat("éñ ", 8192)},
		{"Latin1大写", strings.Repeat("ÉÑ ", 8192)},
		{"Latin1混排", strings.Repeat("éÉñÑ ", 8192)},
		{"希腊文大写", strings.Repeat("ΑΒΓ ", 8192)},
		{"扩展汉字空白", strings.Repeat("\U00020000 ", 8192)},
		{"无效UTF8", strings.Repeat("水\xff杉 ", 8192)},
	}
}

func BenchmarkQQCompactSpans(b *testing.B) {
	for _, tc := range qqCompactSpanCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompact(tc.input)
			}
		})
	}
}
