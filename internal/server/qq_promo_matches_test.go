package server

import (
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var previousQQWeChatPattern = regexp.MustCompile(`(?i)(?:微信|v信|vx)\s*((?:号码?|账号)?\s*[:：]?)\s*([A-Za-z0-9][-_A-Za-z0-9]{4,19})`)

// 保留旧的全量匹配流程，核对分批读取后的分类及非重叠匹配规则。
func previousQQPromoSentenceMatches(sentence string) string {
	if !qqPromoSentenceCandidate(sentence) {
		return ""
	}
	if qqGroupNumberPattern.MatchString(sentence) {
		return "group"
	}
	for _, m := range previousQQWeChatPattern.FindAllStringSubmatchIndex(sentence, -1) {
		if start := m[0]; start > 0 {
			if c := sentence[start-1] | 0x20; c >= 'a' && c <= 'z' {
				continue
			}
		}
		separator, id := strings.TrimSpace(sentence[m[2]:m[3]]), sentence[m[4]:m[5]]
		if separator != "" || strings.ContainsAny(id, "0123456789") {
			return "wechat"
		}
	}
	for _, m := range qqDomainPattern.FindAllStringSubmatchIndex(sentence, -1) {
		host := strings.ToLower(sentence[m[2]:m[3]])
		if !slices.ContainsFunc(qqAllowedDomains, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) }) {
			return "domain"
		}
	}
	return ""
}

func TestQQPromoMatchTraversalCompatibility(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"", ""},
		{"普通说明", ""},
		{"微信 Windows", ""},
		{"avxabc12", ""},
		{"   " + strings.Repeat("avxabc12 ", 17), ""},
		{"vx Windows avxabc12", ""},
		{strings.Repeat("vx Windows ", 17) + "vx:abcde", "wechat"},
		{"vx Windows avxabc12 vxabc34", "wechat"},
		{"微信 Windows vx:abcde", "wechat"},
		{"\xffvxabc12", "wechat"},
		{"VX\t账号\f：\rabcde", "wechat"},
		{"微信号码 abcde", "wechat"},
		{"v信:abcde", "wechat"},
		{"vx\vabc12", ""},
		{"vx\u3000abc12", ""},
		{"vx:abcd", ""},
		{"vx:abcde", "wechat"},
		{"vx:ſKabc", "wechat"},
		{"vxſKabc", ""},
		{"vxſKab1", "wechat"},
		{"ſvxabc12", "wechat"},
		{"avx:abc12", ""},
		{"avxvxabc12", ""},
		{"vx" + strings.Repeat("a", 20) + "1", ""},
		{"vx" + strings.Repeat("a", 19) + "1", "wechat"},
		{"vx" + strings.Repeat("a", 18) + "vxabc12", ""},
		{"github.com", ""},
		{"GITHUB.COM DOCS.GITHUB.COM", ""},
		{"github.com external.example.net", "domain"},
		{strings.Repeat("github.com ", 8) + "external.example.net", "domain"},
		{strings.Repeat("DOCS.GITHUB.COM ", 17) + "external.example.net", "domain"},
		{"github.com:external.example.net", "domain"},
		{"github.comexternal.example.net", "domain"},
		{"github.com\xffexternal.example.net", "domain"},
		{"external.example.net 微信号 abc12", "wechat"},
		{"微信号 abc12 外站.example.net 加群 12345", "group"},
		{strings.Repeat("github.com ", 4096), ""},
	} {
		if got := qqPromoSentence(tc.text); got != tc.want {
			t.Fatalf("引流分类=%q，期望 %q；输入=%q", got, tc.want, tc.text)
		}
		if got := previousQQPromoSentenceMatches(tc.text); got != tc.want {
			t.Fatalf("旧分类基准=%q，期望 %q；输入=%q", got, tc.want, tc.text)
		}
	}
	random := rand.New(rand.NewSource(67))
	units := []string{"普通", "微信 Windows", "vxabc12", "vx abcde", "vx:abcde", "avxabc12", "v信号 abc12", "微信账号 abc12", "github.com", "DOCS.GITHUB.COM", "external.example.net", "加群 12345", "a", " ", "\n", "\t", ":", ".", "。", "\xff", "\xe4\xb8", "😀", "v", "V", "x", "X", "微", "信", "号码", "号", "账号", "：", "\f", "\r", "\v", "\u3000", "ſ", "K", "abcde", "abc12", "abcd", "1", "_", "-"}
	for range 10000 {
		var text strings.Builder
		for range random.Intn(24) {
			text.WriteString(units[random.Intn(len(units))])
		}
		input := text.String()
		if got, want := qqPromoSentence(input), previousQQPromoSentenceMatches(input); got != want {
			t.Fatalf("匹配遍历不兼容：结果=%q，期望 %q；输入=%q", got, want, input)
		}
	}
}

func TestQQPromoMatchAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"wechat", strings.Repeat("微信号 abc12 ", 4096), "wechat"},
		{"domain", strings.Repeat("external.example.com ", 4096), "domain"},
		{"ignored_wechat", strings.Repeat("avxabc12 ", 4096), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got = qqPromoSentence(tc.text)
				}
			})
			if got != tc.want {
				t.Fatalf("分类=%q，期望 %q", got, tc.want)
			}
			t.Logf("%s：%s，%s", tc.name, result.String(), result.MemString())
			// 竞态检测会随机丢弃正则对象池并重建位图；沿用现有过滤测试的做法，在普通模式检查分配预算。
			if !raceEnabled && result.AllocedBytesPerOp() > 4096 {
				t.Fatalf("引流分类扫描分配 %d 字节，超过 4096 字节预算", result.AllocedBytesPerOp())
			}
		})
	}
}

func BenchmarkQQPromoMatchTraversal(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"allowed_domains", strings.Repeat("github.com ", 4096)},
		{"ignored_wechat", strings.Repeat("avxabc12 ", 4096)},
	} {
		for _, implementation := range []struct {
			name string
			run  func(string) string
		}{
			{"previous", previousQQPromoSentenceMatches},
			{"current", qqPromoSentence},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				var got string
				for b.Loop() {
					got = implementation.run(tc.text)
				}
				if got != "" {
					b.Fatal("无引流正文分类错误：", got)
				}
			})
		}
	}
}
