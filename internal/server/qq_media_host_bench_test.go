package server

import (
	"strings"
	"testing"
)

func BenchmarkQQMediaHostAllowed(b *testing.B) {
	for _, tc := range []struct{ name, host string }{
		{"lower", "multimedia.qq.com.cn"},
		{"upper", "MULTIMEDIA.QQ.COM.CN"},
		{"unicode", "例子.qq.com"},
		{"miss", "example.test"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("previous", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = previousQQMediaHostAllowed(tc.host, qqMediaHosts)
				}
			})
			b.Run("current", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = qqMediaHostAllowed(tc.host, qqMediaHosts)
				}
			})
		})
	}
}

func TestQQMediaHostAllowedCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		want       bool
	}{
		{"lower", "multimedia.qq.com.cn", true},
		{"upper", "MULTIMEDIA.QQ.COM.CN", true},
		{"bare_suffix", "qpic.cn", true},
		{"lookalike", "evilqq.com", false},
		{"miss", "example.test", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqMediaHostAllowed(tc.host, qqMediaHosts); got != tc.want {
				t.Fatalf("主机校验=%v，期望=%v", got, tc.want)
			}
		})
	}
}

func previousQQMediaHostAllowed(host string, hosts []string) bool {
	host = strings.ToLower(host)
	for _, suffix := range hosts {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
