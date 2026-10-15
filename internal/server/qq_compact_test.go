package server

import (
	"math/rand"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestQQCompactMatchesPreviousImplementation(t *testing.T) {
	previous := func(text string) string {
		if qqCompactCanReuse(text) {
			return text
		}
		compact := strings.TrimPrefix(strings.ToLower(strings.Join(strings.Fields(qqFoldWidth(text)), "")), "/")
		return qqTrimCommandTrailing(compact)
	}
	random := rand.New(rand.NewSource(13))
	units := []string{"", "a", "A", "1", "/", "?", "！", "🙏", "\u200d", "\u3000", "\t", "\n", "水", "Ｈ", "é", "\xff", "\u0085"}
	for range 4000 {
		var text strings.Builder
		for range random.Intn(100) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		if got, want := qqCompact(input), previous(input); got != want {
			t.Fatalf("命令规范化不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQCompactConditionalRules(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "候选框在哪里", "候选框在哪里"},
		{"spaces", "候选 框 在哪 里", "候选框在哪里"},
		{"case", "Help", "help"},
		{"slash", "/help", "help"},
		{"fullwidth", "帮助？", "帮助"},
		{"trailing_emoji", "帮助🙏", "帮助"},
		{"question_mark", "?", "?"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqCompact(tc.input); got != tc.want {
				t.Fatalf("命令规范化结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQCompactNoopAllocationBudget(t *testing.T) {
	input := strings.Repeat("候选框在哪里", 1024)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := qqCompact(input)
	runtime.ReadMemStats(&after)
	if got != input {
		t.Fatal("普通消息发生变化")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 64<<10 {
		t.Fatalf("普通消息命令规范化分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}

func BenchmarkQQCompact(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"short_plain", "候选框在哪里"},
		{"long_plain", strings.Repeat("候选框在哪里", 1024)},
		{"spaces", strings.Repeat("候选 框 在哪 里 ", 1024)},
		{"command", "帮助？"},
		{"emoji", "最新版本🙏"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompact(tc.input)
			}
		})
	}
}

func BenchmarkQQCompactTrailing(b *testing.B) {
	for _, count := range []int{128, 1024, 8192} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			input := "帮助" + strings.Repeat("🙏", count)
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompact(input)
			}
		})
	}
}

func TestQQTrimCommandTrailingKeepsRules(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""},
		{"?", "?"},
		{"??", "?"},
		{"🙏🙏", "🙏"},
		{"帮助🙏\ufe0f\u200d👍🏽?!。~…", "帮助"},
		{"帮🙏助?", "帮🙏助"},
		{"甲\u0301?", "甲\u0301"},
		{"帮助？", "帮助？"},
		{"\xff?", "\xff"},
		{"甲\xff", "甲"},
		{"甲\ufffd", "甲"},
		{"?\xe4\xb8", "?"},
		{"帮助" + strings.Repeat("🙏", 8192), "帮助"},
	} {
		if got := qqTrimCommandTrailing(tc.input); got != tc.want {
			t.Errorf("尾部字符裁剪不兼容：输入=%q，结果=%q，预期=%q", tc.input, got, tc.want)
		}
	}
}

func TestQQTrimCommandTrailingMatchesPreviousLoop(t *testing.T) {
	units := []string{"a", "水", "?", "。", "🙏", "🏽", "\ufe0f", "\u200d", "\xff", "\xe4\xb8"}
	for _, first := range units {
		for _, middle := range units {
			for _, last := range units {
				input := first + middle + last
				want := input
				for utf8.RuneCountInString(want) > 1 {
					r, size := utf8.DecodeLastRuneInString(want)
					if !strings.ContainsRune(qqCommandTrailing, r) && !unicode.In(r, unicode.So, unicode.Sk) && r != 0xFE0F && r != 0x200D {
						break
					}
					want = want[:len(want)-size]
				}
				if got := qqTrimCommandTrailing(input); got != want {
					t.Fatalf("尾部字符裁剪不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
				}
			}
		}
	}
}
