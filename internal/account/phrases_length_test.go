package account

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

var utf16LengthResult int

func TestUTF16LengthMatchesEncoding(t *testing.T) {
	for _, text := range []string{
		"", "ASCII\x00", "水杉云", "a字😀z", "\ud7ff\ue000\uffff", "\U00010000\U0010ffff",
		"\xff", "\xc0\xaf", "\xed\xa0\x80", "\xf0\x90", "字\xff😀",
	} {
		if got, want := utf16Length(text), len(utf16.Encode([]rune(text))); got != want {
			t.Errorf("%q 的 UTF-16 长度为 %d，期望 %d", text, got, want)
		}
	}
}

func TestUTF16LengthDoesNotAllocate(t *testing.T) {
	for name, text := range map[string]string{
		"ascii":   strings.Repeat("a", 2000),
		"chinese": strings.Repeat("字", 2000),
		"emoji":   strings.Repeat("😀", 1000),
	} {
		t.Run(name, func(t *testing.T) {
			if got := testing.AllocsPerRun(20, func() { utf16LengthResult = utf16Length(text) }); got != 0 {
				t.Fatalf("仅统计长度分配了 %g 次，期望 0 次", got)
			}
			if utf16LengthResult != 2000 {
				t.Fatalf("UTF-16 长度为 %d，期望 2000", utf16LengthResult)
			}
		})
	}
}

func BenchmarkUTF16Length(b *testing.B) {
	for name, text := range map[string]string{
		"ascii":   strings.Repeat("a", 2000),
		"chinese": strings.Repeat("字", 2000),
		"emoji":   strings.Repeat("😀", 1000),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				utf16LengthResult = utf16Length(text)
			}
		})
	}
}

func BenchmarkPhraseValidationLargeList(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		count      int
	}{
		{"ascii", strings.Repeat("a", 2000), 100},
		{"chinese", strings.Repeat("字", 1000), 80},
		{"emoji", strings.Repeat("😀", 1000), 60},
	} {
		b.Run(tc.name, func(b *testing.B) {
			list := make([]Phrase, tc.count)
			for i := range list {
				list[i] = Phrase{ID: fmt.Sprint(i), Text: tc.text}
			}
			// 基准列表必须能通过真实接口的请求体大小与内容校验。
			raw, err := json.Marshal(map[string]any{"revision": 0, "phrases": list})
			if err != nil || len(raw) > maximumPhrasesBytes || !validPhrases(list) {
				b.Fatalf("基准列表无效：%d 字节，错误 %v", len(raw), err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if !validPhrases(list) {
					b.Fatal("常用语校验失败")
				}
			}
		})
	}
}
