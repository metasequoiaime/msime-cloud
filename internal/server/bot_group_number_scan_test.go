package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

var previousQQGroupNumberPattern = regexp.MustCompile(`(?i)(加|进)?\s*(QQ)?\s*群\s*号?\s*[:：]?\s*\d{5,}`)

func TestQQPromoGroupNumberMatchesPattern(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"", false},
		{"普通说明 12345", false},
		{"群", false},
		{"群12345", true},
		{"群" + strings.Repeat("1", 1<<20), true},
		{"QQ群号：12345678", true},
		{"p群12345abc", true},
		{"加qq \t群号 12345", true},
		{"微信群12345", true},
		{"群号码12345", false},
		{"群號12345", false},
		{"羣12345", false},
		{"群号号12345", false},
		{"群号 \f:\t12345", true},
		{"群：1234", false},
		{"群1 2345", false},
		{"群\v12345", false},
		{"群\u300012345", false},
		{"群１２３４５", false},
		{"群١٢٣٤٥", false},
		{"群:1234 群00000", true},
		{"\xff群12345", true},
		{"群\xff12345", false},
		{"vxabc12 external.example.net 群:12345", true},
	} {
		if got := qqPromoSentence(tc.text) == "group"; got != tc.want {
			t.Fatalf("群号判断变化：输入=%q，结果=%v，期望=%v", tc.text, got, tc.want)
		}
	}
	random := rand.New(rand.NewSource(1055))
	units := []string{"普通", "群", "加", "进", "QQ", "qq", "号", "号码", ":", "：", "1", "1234", "12345", "00000", "１２３４５", "a", " ", "\t", "\r", "\n", "\f", "\v", "\u00a0", "\u3000", "\xff", "\xe7", "\xbe", "\xa4", "vxabc12", "external.example.net"}
	for range 10000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		if got, want := qqPromoSentence(input) == "group", previousQQGroupNumberPattern.MatchString(input); got != want {
			t.Fatalf("随机群号判断不兼容：输入=%q，结果=%v，期望=%v", input, got, want)
		}
	}
}

func FuzzQQGroupNumberPromoScan(f *testing.F) {
	for _, input := range []string{"", "QQ群号：12345", "\xff群12345", "群\v12345", "群号号12345", "群1234 群12345", "群\u300012345"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if got, want := qqGroupNumberPromo(input), previousQQGroupNumberPattern.MatchString(input); got != want {
			t.Fatalf("群号扫描不兼容：输入=%q，结果=%v，期望=%v", input, got, want)
		}
	})
}

func BenchmarkQQGroupNumberScan(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", "普通回答内容"},
		{"group", "QQ群号：12345"},
		{"without_group", "vx:abc12 " + strings.Repeat("普通正文", 8192)},
		{"late_group", strings.Repeat("普通正文", 8192) + "群12345"},
		{"dense_groups", strings.Repeat("群聊 ", 8192)},
		{"spaces", "群" + strings.Repeat(" ", 1<<16) + "12345"},
		{"long_digits", "群" + strings.Repeat("1", 1<<20)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) bool
		}{{"previous", previousQQGroupNumberPattern.MatchString}, {"current", qqGroupNumberPromo}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input)
				}
			})
		}
	}
}

func BenchmarkQQPromoGroupNumberFlow(b *testing.B) {
	input := "vx:abc12 " + strings.Repeat("普通正文", 8192)
	b.ReportAllocs()
	for b.Loop() {
		_ = qqPromoSentence(input)
	}
}

func TestQQPromoGroupNumberUnmatchedBudget(t *testing.T) {
	input := "vx:abc12 " + strings.Repeat("普通正文", 8192)
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqPromoSentence(input)
		}
	})
	if got != "wechat" {
		t.Fatalf("引流分类变化：结果=%q，期望 wechat", got)
	}
	// 长正文没有群号时应迅速进入微信号判断，宽松预算允许运行环境波动。
	if result.NsPerOp() > 100_000 {
		t.Fatalf("无群号时的分类扫描过慢：%s，预算 100000 ns/op", result)
	}
}
