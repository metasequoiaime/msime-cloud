package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// telegramLength 保留独立的参考计数，供公告截断差分测试使用。
func telegramLength(text string) int {
	n := 0
	for _, r := range text {
		if size := utf16.RuneLen(r); size > 0 {
			n += size
		} else {
			n++
		}
	}
	return n
}

func previousTelegramText(n account.Notice) string {
	text := n.Title
	if n.Body != "" {
		text += "\n\n" + n.Body
	}
	if telegramLength(text) <= telegramMessageMax {
		return text
	}
	used := 0
	for i, r := range text {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		if used+size > telegramMessageMax-1 {
			return text[:i] + "…"
		}
		used += size
	}
	return text
}

func TestTelegramTextPrefixCompatibility(t *testing.T) {
	check := func(n account.Notice) {
		t.Helper()
		if got, want := telegramText(n), previousTelegramText(n); got != want {
			t.Fatalf("公告截断结果变化：标题字节数=%d，正文字节数=%d，结果=%q，期望=%q", len(n.Title), len(n.Body), got, want)
		}
	}
	for _, title := range []string{"", "水杉😀", "\xe4\xb8", strings.Repeat("😀", 2048)} {
		for _, body := range []string{"", "\x80", "杉", strings.Repeat("😀", 4096)} {
			check(account.Notice{Title: title, Body: body})
		}
	}
	for length := telegramMessageMax - 5; length <= telegramMessageMax+5; length++ {
		for _, title := range []string{strings.Repeat("a", length), strings.Repeat("a", length-2) + "😀"} {
			for _, body := range []string{"", "x", "😀", "\xff\xfe", "\n\n"} {
				check(account.Notice{Title: title, Body: body})
			}
		}
		check(account.Notice{Title: "合成公告", Body: strings.Repeat("a", length) + "😀"})
	}
	check(account.Notice{Title: "合成公告", Body: strings.Repeat("杉", telegramMessageMax-6)})
	check(account.Notice{Title: "合成公告", Body: strings.Repeat("😀", (telegramMessageMax-6)/2)})
	rng := rand.New(rand.NewSource(127))
	for i := 0; i < 256; i++ {
		title, body := make([]byte, rng.Intn(8193)), make([]byte, rng.Intn(20001))
		_, _ = rng.Read(title)
		_, _ = rng.Read(body)
		check(account.Notice{Title: string(title), Body: string(body)})
	}
}

func TestTelegramTextLongBodyAllocationBudget(t *testing.T) {
	for _, unit := range []string{"a", "杉", "😀"} {
		n := account.Notice{Title: "合成公告", Body: strings.Repeat(unit, 20000)}
		var got string
		allocations := testing.AllocsPerRun(20, func() { got = telegramText(n) })
		if got != previousTelegramText(n) {
			t.Fatal("长公告截断结果变化")
		}
		if allocations > 1 {
			t.Fatalf("长公告截断分配 %.0f 次，期望只分配最终前缀结果", allocations)
		}
	}
}

func BenchmarkTelegramTextPrefix(b *testing.B) {
	for _, tc := range []struct {
		name   string
		notice account.Notice
	}{
		{"title_only", account.Notice{Title: "水杉输入法已发布"}},
		{"short", account.Notice{Title: "水杉输入法已发布", Body: "修复了若干问题😀"}},
		{"exact_limit", account.Notice{Title: strings.Repeat("a", telegramMessageMax)}},
		{"unicode_exact_limit", account.Notice{Title: "合成公告", Body: strings.Repeat("杉", telegramMessageMax-6)}},
		{"emoji_exact_limit", account.Notice{Title: "合成公告", Body: strings.Repeat("😀", (telegramMessageMax-6)/2)}},
		{"long_ascii", account.Notice{Title: "合成公告", Body: strings.Repeat("a", 20000)}},
		{"long_chinese", account.Notice{Title: "合成公告", Body: strings.Repeat("杉", 20000)}},
		{"long_emoji", account.Notice{Title: "合成公告", Body: strings.Repeat("😀", 20000)}},
	} {
		for _, impl := range []struct {
			name string
			text func(account.Notice) string
		}{{"previous", previousTelegramText}, {"current", telegramText}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got string
				for b.Loop() {
					got = impl.text(tc.notice)
				}
				if got != previousTelegramText(tc.notice) {
					b.Fatal("公告截断结果变化")
				}
			})
		}
	}
}
