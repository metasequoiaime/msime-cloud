package account

import (
	"math/rand"
	"strings"
	"testing"
)

func TestMaskEmailCompatibility(t *testing.T) {
	cases := []struct {
		name, email, want string
	}{
		{"empty", "", "***"},
		{"no_separator", "user.example.test", "***"},
		{"empty_local", "@example.test", "***"},
		{"empty_domain", "u@", "u***@"},
		{"ascii", "user@example.test", "u***@example.test"},
		{"unicode", "水杉@example.test", "水***@example.test"},
		{"four_bytes", "😀user@example.test", "😀***@example.test"},
		{"replacement", "\ufffduser@example.test", "\ufffd***@example.test"},
		{"invalid_first", "\xffuser@example.test", "\ufffd***@example.test"},
		{"truncated_first", "\xe6@example.test", "\ufffd***@example.test"},
		{"invalid_tail", "水\xff@example.test", "水***@example.test"},
		{"multiple_separators", "u@local@example.test", "u***@example.test"},
		{"separator_as_first_rune", "@@example.test", "@***@example.test"},
		{"raw_domain", "u@\xff", "u***@\xff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskEmail(tc.email); got != tc.want {
				t.Fatalf("打码结果 = %q，期望 %q", got, tc.want)
			}
		})
	}
	rng := rand.New(rand.NewSource(20261016))
	for i := 0; i < 5000; i++ {
		raw := make([]byte, rng.Intn(256))
		if _, err := rng.Read(raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) > 0 && i%2 == 0 {
			raw[rng.Intn(len(raw))] = '@'
		}
		email := string(raw)
		if got, want := maskEmail(email), maskEmailBeforeFirstRune(email); got != want {
			t.Fatalf("第 %d 组字节的打码结果 = %q，期望 %q", i, got, want)
		}
	}
}

func TestMaskEmailAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, local, want string
	}{
		{"ascii_64", strings.Repeat("u", 64), "u***@example.test"},
		{"unicode_63_bytes", strings.Repeat("水", 21), "水***@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			email := tc.local + "@example.test"
			var got string
			allocs := testing.AllocsPerRun(100, func() {
				got = maskEmail(email)
			})
			if got != tc.want {
				t.Fatalf("打码结果 = %q，期望 %q", got, tc.want)
			}
			if allocs > 1 {
				t.Fatalf("每次打码分配 %.0f 次，预算为结果字符串的 1 次分配", allocs)
			}
		})
	}
}

// maskEmailBeforeFirstRune 保留原始实现，用于随机字节差分和配对基准。
func maskEmailBeforeFirstRune(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	first := []rune(email[:at])[0]
	return string(first) + "***" + email[at:]
}

func BenchmarkMaskEmail(b *testing.B) {
	for _, tc := range []struct {
		name, local string
	}{
		{"ascii_short", "user"},
		{"ascii_64", strings.Repeat("u", 64)},
		{"unicode_63_bytes", strings.Repeat("水", 21)},
	} {
		email := tc.local + "@example.test"
		for _, implementation := range []struct {
			name string
			mask func(string) string
		}{
			{"before", maskEmailBeforeFirstRune},
			{"after", maskEmail},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					implementation.mask(email)
				}
			})
		}
	}
}
