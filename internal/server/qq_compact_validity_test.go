package server

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQQCompactValidityKeepsRawFastPath(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"甲\xff乙", "甲\xff乙"},
		{"甲\xff乙?", "甲\ufffd乙"},
		{"甲\ufffd乙?", "甲\ufffd乙"},
		{"甲\xef\xbf乙?", "甲\ufffd\ufffd乙"},
		{"甲\xed\xa0\x80乙?", "甲\ufffd\ufffd\ufffd乙"},
		{"甲\xc0\xaf乙?", "甲\ufffd\ufffd乙"},
		{"甲\xf4\x90\x80\x80乙?", "甲\ufffd\ufffd\ufffd\ufffd乙"},
		{"甲\xf0\x9f\x99乙?", "甲\ufffd\ufffd\ufffd乙"},
		{"甲🙏乙?", "甲🙏乙"},
		{"\xff", "\xff"},
		{"\ufffd", "\ufffd"},
		{"甲\xff?", "甲"},
	} {
		if got := qqCompact(tc.input); got != tc.want {
			t.Errorf("UTF-8 处理变化：输入=%q，结果=%q，期望=%q", tc.input, got, tc.want)
		}
	}
}

func TestQQCompactValidityMatchesPrevious(t *testing.T) {
	check := func(input string) {
		t.Helper()
		if got, want := qqCompactReusable(input, true), utf8.ValidString(input) && referenceQQCompactCanReuse(input); got != want {
			t.Fatalf("UTF-8 复用判定变化：输入=%q，结果=%t，期望=%t", input, got, want)
		}
		if got, want := qqCompact(input), referenceQQCompactSpans(input); got != want {
			t.Fatalf("UTF-8 规范化不兼容：输入=%q，结果=%q，期望=%q", input, got, want)
		}
	}
	// 末尾加汉字，使内部无效字节不会被直接当作尾部符号裁掉。
	for first := range 256 {
		for second := range 256 {
			body := "水" + string([]byte{byte(first), byte(second)}) + "杉"
			check(body)
			check(body + "？")
			check(body + "🙏")
		}
	}
	random := rand.New(rand.NewSource(963))
	for range 3000 {
		body := make([]byte, random.Intn(100))
		_, _ = random.Read(body)
		check("水" + string(body) + "杉？")
		check("水\ufffd" + string(body) + "\ufffd杉🙏")
	}
}

func BenchmarkQQCompactValidity(b *testing.B) {
	body := strings.Repeat("候选框在哪里", 333)
	for _, tc := range []struct{ name, input string }{
		{"短中文命令", "帮助？"},
		{"短ASCII命令", "Help?"},
		{"长中文问号", body + "？"},
		{"长中文表情", body + "🙏"},
		{"长正文首部空白", " " + body + "？"},
		{"长正文首部大写", "HELP " + body + "？"},
		{"长正文首部全角", "ＨＥＬＰ　" + body + "？"},
		{"有效替换字符", "水\ufffd" + body + "杉？"},
		{"内部无效UTF8", "水\xff" + body + "杉？"},
		{"普通长中文", body},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqCompact(tc.input)
			}
		})
	}
}
