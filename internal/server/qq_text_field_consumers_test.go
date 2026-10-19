package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQQCompactLowerASCIIFastPath(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		fast              bool
	}{
		{"spaces", " BODY \tTEXT \n", "bodytext", true},
		{"controls", "A\x00 B\x7f C", "a\x00b\x7fc", true},
		{"plain", "AlreadyLower", "alreadylower", true},
		{"unicode", "BODY 水", "", false},
		{"invalid", "BODY \xff", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, fast := qqCompactLowerASCII(tc.input)
			if fast != tc.fast {
				t.Fatalf("ASCII 快路径标记=%t，期望 %t", fast, tc.fast)
			}
			if fast && got != tc.want {
				t.Fatalf("ASCII 快路径结果=%q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestQQCompactLowerBasicFastPath(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		fast              bool
	}{
		{"spaces", " BODY \tTEXT \n", "bodytext", true},
		{"controls", "A\x00 B\x7f C", "a\x00b\x7fc", true},
		{"plain", "AlreadyLower", "alreadylower", true},
		{"han", "BODY 水", "body水", true},
		{"han_plain", "水杉正文", "水杉正文", true},
		{"han_ascii", "水杉BODY", "水杉body", true},
		{"han_boundary", "一 鿿", "一鿿", true},
		{"unicode", "BODY K", "", false},
		{"unicode_space", "甲\u3000乙", "", false},
		{"partial_han", "水杉\xe4\xb8", "", false},
		{"invalid_han", "水杉\xe4\xb8x", "", false},
		{"invalid", "BODY \xff", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, fast := qqCompactLowerBasic(tc.input)
			if fast != tc.fast {
				t.Fatalf("基础字符快路径标记=%t，期望 %t", fast, tc.fast)
			}
			if fast && got != tc.want {
				t.Fatalf("基础字符快路径结果=%q，期望 %q", got, tc.want)
			}
		})
	}
}

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

func TestQQCompactLowerFastPathMatchesStandardLibrary(t *testing.T) {
	check := func(input string) {
		t.Helper()
		want := strings.ToLower(strings.Join(strings.Fields(input), ""))
		if got, ok := qqCompactLowerBasic(input); ok && got != want {
			t.Fatalf("快路径结果变化：输入=%q，结果=%q，期望=%q", input, got, want)
		}
		if got := qqCompactLower(input); got != want {
			t.Fatalf("去空白转小写结果变化：输入=%q，结果=%q，期望=%q", input, got, want)
		}
	}
	// 穷举码点，覆盖汉字范围边界及其他 Unicode 字符的回退行为。
	for r := rune(0); r <= utf8.MaxRune; r++ {
		check("水杉 A " + string(r) + " B 正文")
	}
	random := rand.New(rand.NewSource(1009))
	for range 4000 {
		raw := make([]byte, random.Intn(128))
		_, _ = random.Read(raw)
		check("水杉 A " + string(raw) + " B 正文")
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
				{"han_ascii_upper", strings.Repeat("普通 BODY ", 4096)},
				{"han_plain", strings.Repeat("普通正文", 4096)},
				{"han_unicode_tail", strings.Repeat("普通 正文 ", 4096) + "K"},
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
