package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"
)

func previousCloudUTF16Length(text string) int {
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
		if got, want := utf16Length(text), previousCloudUTF16Length(text); got != want {
			t.Fatalf("UTF-16 长度变化：输入=%q，结果=%d，期望=%d", text, got, want)
		}
	}
}

var cloudUTF16LengthResult int

func BenchmarkCloudUTF16Length(b *testing.B) {
	for _, impl := range []struct {
		name string
		run  func(string) int
	}{{"previous", previousCloudUTF16Length}, {"current", utf16Length}} {
		b.Run(impl.name, func(b *testing.B) {
			for _, tc := range []struct{ name, text string }{
				{"短拼音", "ni'hao-"},
				{"长度上限ASCII", strings.Repeat("a-", 128)},
				{"补充平面字符", strings.Repeat("😀", 64)},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						cloudUTF16LengthResult = impl.run(tc.text)
					}
				})
			}
		})
	}
}
