package server

import (
	"strings"
	"testing"
)

func TestQQCompactTrailingPrefixValidation(t *testing.T) {
	for _, tc := range []struct {
		input, prefix, compact string
	}{
		{"", "", ""},
		{"帮助", "", "帮助"},
		{"帮助🙏", "帮助", "帮助"},
		{"甲?", "甲", "甲"},
		{"?", "", "?"},
		{"?!", "", "?"},
		{"🙏🙏", "", "🙏"},
		{"/?", "", "?"},
		{" / \t\u3000?", "", "?"},
		{"／?", "", "?"},
		{" ／ \u3000？ ", "", "?"},
		{"／🙏", "", "🙏"},
		{"／?!", "", "?"},
		{"//?", "//", "/"},
		{"／/?", "／/", "/"},
		{"/甲?", "/甲", "甲"},
		{"／甲?", "／甲", "甲"},
		{" \u3000甲 \t?", " \u3000甲", "甲"},
		{"A B🙏", "A B", "ab"},
		{"甲\xff?", "甲\xff", "甲"},
	} {
		prefix, ok := qqCompactTrailingPrefix(tc.input)
		if prefix != tc.prefix || ok != (tc.prefix != "") {
			t.Errorf("前缀判定变化：输入=%q，结果=%q/%t，期望=%q/%t", tc.input, prefix, ok, tc.prefix, tc.prefix != "")
		}
		if got := qqCompact(tc.input); got != tc.compact {
			t.Errorf("命令规范化变化：输入=%q，结果=%q，期望=%q", tc.input, got, tc.compact)
		}
	}
}

func TestQQCompactEarlyPrefixStillNormalizesRemainder(t *testing.T) {
	body := strings.Repeat("普通正文", 1024)
	for _, tc := range []struct{ suffix, want string }{
		{"HELP🙏", "help"},
		{"ＨＥＬＰ🙏", "help"},
		{" 帮 助🙏", "帮助"},
		{"\xffHELP🙏", "\ufffdhelp"},
	} {
		if got, want := qqCompact(body+tc.suffix), body+tc.want; got != want {
			t.Fatalf("后续正文未正确规范化：后缀=%q，结果长度=%d，期望长度=%d", tc.suffix, len(got), len(want))
		}
	}
}

func qqCompactPrefixValidationCases() []struct{ name, input string } {
	body := strings.Repeat("候选框在哪里", 8192)
	return []struct{ name, input string }{
		{"短命令", "帮助？"},
		{"长正文带表情", body + "🙏"},
		{"长正文带标点", body + "？"},
		{"带斜杠长正文", "/" + body + "🙏"},
		{"前导空白长正文", strings.Repeat(" \t\u3000", 8192) + body + "🙏"},
		{"全空白前缀", strings.Repeat(" \t\u3000", 8192) + "🙏"},
		{"长尾部表情", "帮助" + strings.Repeat("🙏", 8192)},
		{"无尾部标点", body},
	}
}

func BenchmarkQQCompactPrefixValidation(b *testing.B) {
	for _, tc := range qqCompactPrefixValidationCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("前缀检查", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = qqCompactTrailingPrefix(tc.input)
				}
			})
			b.Run("完整规范化", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = qqCompact(tc.input)
				}
			})
		})
	}
}
