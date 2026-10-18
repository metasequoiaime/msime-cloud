package server

import (
	"strings"
	"testing"
)

func TestQQTextFieldConsumersKeepStandardLibraryRules(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"ordinary", "普通 正文\t备用 资料\n"},
		{"unicode_spaces", "甲\u0085乙\u00a0丙\u1680丁\u2000戊\u200a己\u2028庚\u2029辛\u202f壬\u205f癸\u3000"},
		{"invalid_bytes", "\xe6 \x8f \x90\xff\xe0\xc2\xa0正文\xf0\xe3\x80\x80尾词 "},
		{"case_mapping", "\u023a\u212a\u0130 BODY\u2003正文 "},
		{"whitespace", " \t\n\r\v\f\u0085\u3000"},
	} {
		for _, length := range []struct {
			name   string
			repeat int
		}{{"short", 1}, {"long", 16384}} {
			t.Run(tc.name+"/"+length.name, func(t *testing.T) {
				input := strings.Repeat(tc.text, length.repeat)
				fields := strings.Fields(input)
				if got, want := qqNormalizeWhitespace(input), strings.Join(fields, " "); got != want {
					t.Fatalf("空白归一化结果发生变化：结果长度=%d，预期长度=%d", len(got), len(want))
				}
				// 先合并字节再转小写，保留无效片段重新组成有效 UTF-8 的行为。
				if got, want := qqCompactLower(input), strings.ToLower(strings.Join(fields, "")); got != want {
					t.Fatalf("去空白转小写结果发生变化：结果长度=%d，预期长度=%d", len(got), len(want))
				}
			})
		}
	}
}

func BenchmarkQQTextFieldConsumers(b *testing.B) {
	for _, consumer := range []struct {
		name string
		run  func(string) string
	}{{"normalize", qqNormalizeWhitespace}, {"compact_lower", qqCompactLower}} {
		b.Run(consumer.name, func(b *testing.B) {
			for _, tc := range []struct{ name, text string }{
				{"short", "普通 正文"},
				{"chinese", strings.Repeat("普通 正文 ", 4096)},
				{"ascii", strings.Repeat("ordinary text ", 4096)},
				{"unicode_spaces", strings.Repeat("甲\u3000乙\u0085丙 ", 4096)},
				{"long", strings.Repeat("普通 正文 ", 16384)},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = consumer.run(tc.text)
					}
				})
			}
		})
	}
}
