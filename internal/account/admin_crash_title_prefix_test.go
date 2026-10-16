package account

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// 保留原有的完整计数和整行转换，核对截断、空白及非法 UTF-8 的兼容性。
func previousCrashGroupTitle(message string) string {
	line := crashFirstLine(message)
	if utf8.RuneCountInString(line) > 200 {
		line = strings.TrimSpace(string([]rune(line)[:200]))
	}
	return line
}

func TestCrashGroupTitlePrefixCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, message, want string }{
		{"Empty", "\n \t\r\n", ""},
		{"FirstNonblank", "\n\t\r\n first\u3000\nsecond", "first"},
		{"ExactLimit", strings.Repeat("水😀", 100), strings.Repeat("水😀", 100)},
		{"OverLimit", strings.Repeat("水😀", 101), strings.Repeat("水😀", 100)},
		{"TrimAtCut", strings.Repeat("水", 199) + "\u3000tail", strings.Repeat("水", 199)},
		{"InvalidShort", "\xff\xfetail", "\xff\xfetail"},
		{"InvalidExactLimit", strings.Repeat("水", 198) + "\xff\xfe", strings.Repeat("水", 198) + "\xff\xfe"},
		{"InvalidTruncated", strings.Repeat("水", 198) + "\xff\xfetail", strings.Repeat("水", 198) + "\ufffd\ufffd"},
		{"BrokenSequenceAtCut", strings.Repeat("水", 199) + "\xf0\x9f\x98tail", strings.Repeat("水", 199) + "\ufffd"},
		{"InvalidAfterCut", strings.Repeat("水", 200) + "\xff\xfe", strings.Repeat("水", 200)},
		{"ReplacementRune", strings.Repeat("\ufffd", 201), strings.Repeat("\ufffd", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := crashGroupTitle(tc.message); got != tc.want {
				t.Fatalf("标题=%q，期望=%q", got, tc.want)
			}
		})
	}
	random := rand.New(rand.NewSource(689))
	atoms := []string{"a", "水", "😀", "\ufffd", "\xff", "\xfe", "\xf0\x9f", "\x00", "\t", "\v", "\r", "\u3000", "\u00a0", "\u200b"}
	for range 1000 {
		var message strings.Builder
		message.WriteString("\n \t\r\n")
		for range random.Intn(300) {
			message.WriteString(atoms[random.Intn(len(atoms))])
		}
		message.WriteString("\nsecond line")
		text := message.String()
		if got, want := crashGroupTitle(text), previousCrashGroupTitle(text); got != want {
			t.Fatalf("消息=%q，标题=%q，原标题=%q", text, got, want)
		}
	}
}

func TestCrashGroupTitlePrefixAvoidsCopies(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 2048), strings.Repeat("水😀", 1024)} {
		want := previousCrashGroupTitle(text)
		allocs := testing.AllocsPerRun(100, func() {
			if got := crashGroupTitle(text); got != want {
				t.Fatalf("标题=%q，原标题=%q", got, want)
			}
		})
		if allocs != 0 {
			t.Fatalf("截取合法 UTF-8 标题分配了 %.0f 次，期望复用前缀而不分配", allocs)
		}
	}
}

var crashTitlePrefixResult string

func BenchmarkCrashGroupTitlePrefix(b *testing.B) {
	for _, tc := range []struct{ name, message string }{
		{"ShortASCII", "synthetic crash message"},
		{"ShortUnicode", strings.Repeat("合成崩溃消息", 10)},
		{"ExactUnicode", strings.Repeat("水", 200)},
		{"LongASCII", strings.Repeat("a", 2048)},
		{"LongUnicode", strings.Repeat("水", 2048)},
		{"InvalidPrefix", strings.Repeat("水", 198) + "\xff\xfe" + strings.Repeat("水", 1848)},
		{"HistoricalLongLine", strings.Repeat("水", 64<<10)},
	} {
		for _, impl := range []struct {
			name  string
			title func(string) string
		}{{"Previous", previousCrashGroupTitle}, {"Current", crashGroupTitle}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashTitlePrefixResult = impl.title(tc.message)
				}
			})
		}
	}
}
