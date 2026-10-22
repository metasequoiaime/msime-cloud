package textutil

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"
)

func previousUTF16Length(text string) int {
	return len(utf16.Encode([]rune(text)))
}

func TestUTF16LengthMatchesEncoding(t *testing.T) {
	texts := []string{"", "ASCII\x00", "水杉云", "a字😀z", "\ud7ff\ue000\uffff", "\U00010000\U0010ffff", "\xff", "\xc0\xaf", "\xed\xa0\x80", "\xf0\x90", "字\xff😀"}
	random := rand.New(rand.NewSource(672))
	for range 3000 {
		text := make([]byte, random.Intn(257))
		_, _ = random.Read(text)
		texts = append(texts, string(text))
	}
	for _, text := range texts {
		if got, want := UTF16Length(text), previousUTF16Length(text); got != want {
			t.Fatalf("UTF-16 长度变化：输入=%q，结果=%d，期望=%d", text, got, want)
		}
	}
}

var utf16LengthResult int

func TestUTF16LengthDoesNotAllocate(t *testing.T) {
	for name, text := range map[string]string{
		"ascii":   strings.Repeat("a", 2000),
		"chinese": strings.Repeat("字", 2000),
		"emoji":   strings.Repeat("😀", 1000),
		"invalid": strings.Repeat("\xff", 2000),
	} {
		t.Run(name, func(t *testing.T) {
			if got := testing.AllocsPerRun(20, func() { utf16LengthResult = UTF16Length(text) }); got != 0 {
				t.Fatalf("仅统计长度分配了 %g 次，期望 0 次", got)
			}
			if utf16LengthResult != 2000 {
				t.Fatalf("UTF-16 长度为 %d，期望 2000", utf16LengthResult)
			}
		})
	}
}

func BenchmarkUTF16Length(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func(string) int
	}{{"previous", previousUTF16Length}, {"current", UTF16Length}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, tc := range []struct{ name, text string }{
				{"短拼音", "ni'hao-"},
				{"长度上限ASCII", strings.Repeat("a-", 128)},
				{"补充平面字符", strings.Repeat("😀", 64)},
				{"长ASCII", strings.Repeat("a", 2000)},
				{"长汉字", strings.Repeat("字", 2000)},
				{"长补充平面字符", strings.Repeat("😀", 1000)},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						utf16LengthResult = impl.run(tc.text)
					}
				})
			}
		})
	}
}
