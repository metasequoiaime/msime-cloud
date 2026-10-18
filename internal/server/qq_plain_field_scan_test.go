package server

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQQPlainTextFieldsMatchStandardLibrary(t *testing.T) {
	for _, input := range []string{
		"", "正文", " \t\n\v\f\r ", "甲 \t乙\n丙\r丁",
		"甲\u0085乙\u00a0丙\u1680丁\u2000戊\u2028己\u202f庚\u205f辛\u3000壬",
		"甲\u200b乙\ufeff丙", "甲\xff\xc2 乙\xe3\x80 丙\xfe",
		"\xe0\xc2\xa0正文\xf0\xe3\x80\x80尾词", "\x00甲\x7f 乙",
	} {
		got := slices.Collect(qqPlainTextFields(input))
		if want := strings.Fields(input); !slices.Equal(got, want) {
			t.Fatalf("分词不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQPlainTextFieldsMatchEveryRune(t *testing.T) {
	// 覆盖全部码点，避免首字节筛选遗漏 Unicode 空白或误拆普通字符。
	for r := rune(0); r <= utf8.MaxRune; r++ {
		input := "甲" + string(r) + "乙"
		got := slices.Collect(qqPlainTextFields(input))
		if want := strings.Fields(input); !slices.Equal(got, want) {
			t.Fatalf("码点 %U 的分词不兼容：结果=%q，预期=%q", r, got, want)
		}
	}
}

func TestQQPlainTextFieldsMatchArbitraryBytes(t *testing.T) {
	random := rand.New(rand.NewPCG(71, 29))
	for range 2048 {
		data := make([]byte, random.IntN(256))
		for i := range data {
			data[i] = byte(random.Uint32())
		}
		input := string(data)
		got := slices.Collect(qqPlainTextFields(input))
		if want := strings.Fields(input); !slices.Equal(got, want) {
			t.Fatalf("任意字节的分词不兼容：输入=%q，结果=%q，预期=%q", input, got, want)
		}
	}
}

func TestQQPlainTextFieldsStopsAndRestarts(t *testing.T) {
	for _, input := range []string{"甲 乙 丙", "甲\u3000乙", "甲", ""} {
		fields := qqPlainTextFields(input)
		calls := 0
		fields(func(string) bool {
			calls++
			return false
		})
		if want := min(len(strings.Fields(input)), 1); calls != want {
			t.Fatalf("迭代器未及时停止：调用=%d，预期=%d", calls, want)
		}
		if got, want := slices.Collect(fields), strings.Fields(input); !slices.Equal(got, want) {
			t.Fatalf("重复遍历结果=%q，预期=%q", got, want)
		}
	}
}

func BenchmarkQQPlainTextFields(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"mixed_runs", strings.Repeat("词 \t \t", 32768)},
		{"chinese", strings.Repeat("普通正文 ", 4096)},
		{"ascii", strings.Repeat("ordinary text ", 4096)},
		{"unicode_spaces", strings.Repeat("甲\u3000乙\u0085丙 ", 4096)},
		{"short", "正文 尾词"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for range qqPlainTextFields(tc.input) {
				}
			}
		})
	}
}
