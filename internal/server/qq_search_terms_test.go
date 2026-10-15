package server

import (
	"math/rand"
	"strings"
	"testing"
)

func TestQQSearchTermsKeepsSeparatorsAndLimit(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", ""},
		{" \t\n:\"\\\u3000\u0085", ""},
		{"候选框", "候选框"},
		{"  候选框\t光标\n跳动  ", "候选框 光标 跳动"},
		{`repo:other/x "is:closed" foo\bar 忽略`, "repo other/x is closed foo bar"},
		{"一 二 三 四 五 六 七 八", "一 二 三 四 五 六"},
		{"a\u0085b\u00a0c\u2003d\u2028e\u3000f tail", "a b c d e f"},
		{"a\u200bb\ufeffc\x00d", "a\u200bb\ufeffc\x00d"},
		{"repo：x ＂is：closed＂ ＼", "repo：x ＂is：closed＂ ＼"},
		{"\xff\xfe:\xe4\xb8 \xed\xa0\x80", "\xff\xfe \xe4\xb8 \xed\xa0\x80"},
	} {
		if got := qqSearchTerms(tc.input); got != tc.want {
			t.Fatalf("搜索词不一致：输入=%q，结果=%q，期望=%q", tc.input, got, tc.want)
		}
	}
}

func TestQQSearchTermsMatchesPreviousFormat(t *testing.T) {
	random := rand.New(rand.NewSource(17))
	for i := range 500 {
		data := make([]byte, random.Intn(2048))
		_, _ = random.Read(data)
		input := string(data)
		if i%2 == 0 {
			input = "候选框:光标 \"repo:other/x\" is\\closed " + input
		}
		terms := strings.Fields(strings.NewReplacer(":", " ", `"`, " ", "\\", " ").Replace(input))
		want := strings.Join(terms[:min(len(terms), 6)], " ")
		if got := qqSearchTerms(input); got != want {
			t.Fatalf("旧格式对照不一致：序号=%d，输入字节数=%d，结果=%q，期望=%q", i, len(input), got, want)
		}
	}
}

func TestQQSearchTermsUsesBoundedMemory(t *testing.T) {
	input := "候选框 光标 repo:other/x \"is:closed\" " + strings.Repeat("discard\\:token ", 65536)
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqSearchTerms(input)
		}
	})
	if want := "候选框 光标 repo other/x is closed"; got != want {
		t.Fatalf("搜索词结果=%q，期望=%q", got, want)
	}
	if allocated := result.AllocedBytesPerOp(); allocated > 4<<10 {
		t.Fatalf("搜索词分配 %d 字节，超过 4 KiB 预算", allocated)
	}
}

func BenchmarkQQSearchTerms(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"empty", ""},
		{"single", "候选框"},
		{"typical", "候选框 光标 跳动 Windows 设置"},
		{"filtered", `候选框 光标 repo:other/x "is:closed" 忽略`},
		{"model_limit", strings.Repeat("a:", qqBotFieldRunes/2)},
		{"long_tail", "一 二 三 四 五 六 " + strings.Repeat("discard\\:token ", 65536)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqSearchTerms(tc.input)
			}
		})
	}
}
