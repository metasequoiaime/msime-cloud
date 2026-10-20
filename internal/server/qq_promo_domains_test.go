package server

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// previousQQDomainPromo 保留分批正则匹配和小写比较，作为直接扫描的性能基准。
func previousQQDomainPromo(text string) bool {
	for remaining := text; remaining != ""; {
		matches := previousQQDomainPattern.FindAllStringSubmatchIndex(remaining, 8)
		if len(matches) == 0 {
			break
		}
		for _, m := range matches {
			host := strings.ToLower(remaining[m[2]:m[3]])
			if !slices.ContainsFunc(qqAllowedDomains, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) }) {
				return true
			}
		}
		remaining = remaining[matches[len(matches)-1][1]:]
	}
	return false
}

func checkQQPromoDomainScan(t testing.TB, input string) {
	t.Helper()
	wantDomain := false
	offset := 0
	for _, m := range previousQQDomainPattern.FindAllStringSubmatchIndex(input, -1) {
		start, end, next, ok := qqNextPromoDomain(input, offset)
		if !ok || start != m[0] || end != m[3] || next != m[1] || next <= offset {
			t.Fatalf("域名匹配区间变化：输入=%q，结果=(%d,%d,%d,%v)，期望=(%d,%d,%d,true)", input, start, end, next, ok, m[0], m[3], m[1])
		}
		host := strings.ToLower(input[m[2]:m[3]])
		allowed := slices.ContainsFunc(qqAllowedDomains, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) })
		if got := qqPromoDomainAllowed(input[start:end]); got != allowed {
			t.Fatalf("允许域名比较变化：域名=%q，结果=%v，期望=%v", input[start:end], got, allowed)
		}
		wantDomain = wantDomain || !allowed
		offset = next
	}
	if start, end, next, ok := qqNextPromoDomain(input, offset); ok {
		t.Fatalf("出现额外域名匹配：输入=%q，区间=(%d,%d,%d)", input, start, end, next)
	}
	if got := qqDomainPromo(input); got != wantDomain {
		t.Fatalf("域名引流判断变化：输入=%q，结果=%v，期望=%v", input, got, wantDomain)
	}
}

func TestQQPromoDomainScanCompatibility(t *testing.T) {
	for _, input := range []string{
		"", "普通说明", "github.com", "GITHUB.COM", "DOCS.GITHUB.COM", "external.example.net",
		"github.com:external.example.net", "github.comexternal.example.net", "github.com.unknown",
		"a.com.b.net", "a.com..b.net", "a.com.---b.net", "a.com.-b.net", "a.com.b-", "a.com.b-.net",
		"---github.com", "github-.com", "github.com-", "github.com-extra.net", "x.-com", "x.---.net",
		"x.123.com", "a..github.com", "a.comgithub.com", "r.github.com.msime.app", "a.usx",
		"a.uſ", "a.Kr", "ſite.example.net", "K.github.com", "ſ.github.com", "a.ſhop", "a.webſite",
		"gİthub.com", "gıthub.com", "Ｇithub.com", "a.com\xffb.net", "a.com\xe4\xb8b.net", "a.com😀b.net", "a.com\nb.net",
		"README.md default.custom.yaml Squirrel.app", ".com", "-.com", "a.com.",
		strings.Repeat("label.", 10000) + "unknown", "a.com." + strings.Repeat("label.", 10000) + "unknown",
	} {
		checkQQPromoDomainScan(t, input)
	}
	for _, tld := range strings.Split("com|net|org|cn|xyz|top|io|cc|me|co|info|biz|vip|club|site|online|shop|store|live|pro|fun|icu|cyou|buzz|work|life|world|tech|space|website|link|ltd|ink|wang|xin|asia|hk|tw|tv|ru|us|uk|de|jp|kr|dev|win", "|") {
		for _, input := range []string{"x." + tld, "x." + strings.ToUpper(tld), "x." + tld + "x", "x." + tld + "-", "x." + tld + ".unknown"} {
			checkQQPromoDomainScan(t, input)
		}
	}
	random := rand.New(rand.NewSource(1056))
	units := []string{"普通", "a", "com", "net", "github", "GITHUB", "unknown", "us", "uk", "website", "-", "--", ".", "..", " ", "\n", "\t", "_", "1", "0", ":", "/", "ſ", "K", "İ", "😀", "\xff", "\xe2", "\x84", "\xaa", "\xc5", "\xbf"}
	for range 10000 {
		var input strings.Builder
		for range random.Intn(80) {
			input.WriteString(units[random.Intn(len(units))])
		}
		checkQQPromoDomainScan(t, input.String())
	}
}

func FuzzQQPromoDomainScan(f *testing.F) {
	for _, input := range []string{"", "github.com", "a.com..b.net", "a.com.unknown", "a.com.b-.net", "a.com.-b.net", "a.uſ", "a.Kr", "a.webſite", "a.com\xffb.net", "a.com😀b.net", "gİthub.com"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		checkQQPromoDomainScan(t, input)
	})
}

func TestQQPromoDomainAllowedMatchesLower(t *testing.T) {
	for _, host := range []string{"", "github.com", "GITHUB.COM", "DOCS.GITHUB.COM", "xgithub.com", "github.com.net", "MSIME.APP", "mſime.app", "GİTHUB.COM", "ſ.K.github.com"} {
		lower := strings.ToLower(host)
		want := slices.ContainsFunc(qqAllowedDomains, func(d string) bool { return lower == d || strings.HasSuffix(lower, "."+d) })
		if got := qqPromoDomainAllowed(host); got != want {
			t.Fatalf("小写域名比较变化：输入=%q，结果=%v，期望=%v", host, got, want)
		}
	}
}

func BenchmarkQQPromoDomainScan(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"allowed", strings.Repeat("github.com ", 4096)},
		{"uppercase", strings.Repeat("DOCS.GITHUB.COM ", 4096)},
		{"no_dot", strings.Repeat("avxabc12 ", 4096)},
		{"rejected", strings.Repeat("external.example.net ", 4096)},
		{"unicode", strings.Repeat("ſ.K.github.com。", 4096)},
		{"fallback", "github.com." + strings.Repeat("label.", 4096) + "unknown"},
	} {
		for _, impl := range []struct {
			name string
			run  func(string) bool
		}{{"previous", previousQQDomainPromo}, {"current", qqDomainPromo}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = impl.run(tc.input)
				}
			})
		}
	}
}
