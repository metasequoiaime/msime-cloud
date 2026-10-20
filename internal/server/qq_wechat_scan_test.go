package server

import (
	"math/rand"
	"strings"
	"testing"
)

// previousQQWeChatPromo 保留原来的分批正则遍历，验证匹配消耗和号码判定规则。
func previousQQWeChatPromo(sentence string) bool {
	for offset := 0; offset < len(sentence); {
		remaining := sentence[offset:]
		matches := previousQQWeChatPattern.FindAllStringSubmatchIndex(remaining, 8)
		if len(matches) == 0 {
			break
		}
		for _, m := range matches {
			if start := offset + m[0]; start > 0 {
				if c := sentence[start-1] | 0x20; c >= 'a' && c <= 'z' {
					continue
				}
			}
			separator, id := strings.TrimSpace(remaining[m[2]:m[3]]), remaining[m[4]:m[5]]
			if separator != "" || strings.ContainsAny(id, "0123456789") {
				return true
			}
		}
		offset += matches[len(matches)-1][1]
	}
	return false
}

func TestQQWeChatPromoScanCompatibility(t *testing.T) {
	check := func(input string) {
		t.Helper()
		if got, want := qqWeChatPromo(input), previousQQWeChatPromo(input); got != want {
			t.Fatalf("微信号扫描不兼容：输入=%q，结果=%v，期望=%v", input, got, want)
		}
	}
	for _, prefix := range []string{"微信", "v信", "V信", "vx", "vX", "Vx", "VX"} {
		for _, separator := range []string{"", " ", "\t\r\n\f ", "号", "号码", "账号", ":", "：", " 号 \t：\r", "\v", "\u3000", "\u00a0"} {
			for _, unit := range []string{"a", "1", "ſ", "K", "_", "-", "\xff"} {
				for _, length := range []int{0, 1, 4, 5, 19, 20, 21} {
					input := prefix + separator + strings.Repeat(unit, length)
					check(input)
					check("a" + input + " vxabc12")
				}
			}
		}
	}
	random := rand.New(rand.NewSource(1054))
	units := []string{"微信", "v信", "VX", "Vx", "vx", "wx", "v", "x", "微", "信", "号", "号码", "账号", "：", ":", " ", "\t", "\n", "\f", "\r", "\v", "\u3000", "\u00a0", "a", "1", "abcde", "abc12", "ſ", "K", "_", "-", "\xff", "\xe5", "😀"}
	for range 10000 {
		var input strings.Builder
		for range random.Intn(80) {
			input.WriteString(units[random.Intn(len(units))])
		}
		check(input.String())
	}
}

func FuzzQQWeChatPromoScan(f *testing.F) {
	for _, input := range []string{"", "微信 Windows", "vx\t号码：ſKab1", "avxvxabc12", "vx" + strings.Repeat("a", 18) + "vxabc12", "\xffv信号 abc12"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if got, want := qqWeChatPromo(input), previousQQWeChatPromo(input); got != want {
			t.Fatalf("微信号扫描不兼容：输入=%q，结果=%v，期望=%v", input, got, want)
		}
	})
}

func BenchmarkQQWeChatPromoScan(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", "普通回复内容"},
		{"wechat", "微信号 abc12"},
		{"windows", "微信 Windows 版的操作说明"},
		{"ignored", strings.Repeat("avxabc12 ", 4096)},
		{"late", strings.Repeat("avxabc12 ", 4096) + "微信号 abc12"},
		{"long_plain", strings.Repeat("普通回复内容", 4096)},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) bool
		}{{"previous", previousQQWeChatPromo}, {"current", qqWeChatPromo}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input)
				}
			})
		}
	}
}
