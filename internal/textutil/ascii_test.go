package textutil

import (
	"math/rand"
	"strings"
	"testing"
)

func TestEqualFoldASCII(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"", "", true}, {"A", "a", true}, {"aBc", "AbC", true},
		{"windows", "WINDOWS", true}, {"WAV", "wav", true},
		{"a", "", false}, {"[", "{", false}, {"@", "`", false},
		{"鸿蒙", "鸿蒙", true}, {"鸿草", "鸿蒙", false}, {"鸡菜", "鸿蒙", false},
		{"A中", "a丰", false}, {"中A", "中a", true},
		{"É", "é", false}, {"Σ", "σ", false}, {"Σ", "ς", false},
		{"K", "K", false}, {"S", "ſ", false},
		{"\xffA", "\xffa", true}, {"\xff", "\xfe", false},
		{"\xe4\xb8\x00", "\xe4\xb8\x80", false},
	} {
		if got := EqualFoldASCII(tc.a, tc.b); got != tc.want {
			t.Errorf("ASCII 折叠比较 %q 与 %q：结果=%v，期望=%v", tc.a, tc.b, got, tc.want)
		}
		if got := EqualFoldASCII(tc.b, tc.a); got != tc.want {
			t.Errorf("ASCII 折叠反向比较 %q 与 %q：结果=%v，期望=%v", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestEqualFoldASCIIMatchesByteReplacement(t *testing.T) {
	var pairs []string
	for upper := byte('A'); upper <= 'Z'; upper++ {
		pairs = append(pairs, string(upper), string(upper+'a'-'A'))
	}
	lower := strings.NewReplacer(pairs...)
	check := func(a, b string) {
		t.Helper()
		if got, want := EqualFoldASCII(a, b), lower.Replace(a) == lower.Replace(b); got != want {
			t.Fatalf("与字节替换结果不同：%q 与 %q，结果=%v，期望=%v", a, b, got, want)
		}
	}
	for a := range 256 {
		for b := range 256 {
			check(string([]byte{byte(a)}), string([]byte{byte(b)}))
		}
	}
	random := rand.New(rand.NewSource(20261023))
	for range 1000 {
		a, b := make([]byte, random.Intn(128)), make([]byte, random.Intn(128))
		_, _ = random.Read(a)
		_, _ = random.Read(b)
		check(string(a), string(b))
		check(string(a), lower.Replace(string(a)))
		if len(a) > 0 {
			b = append(b[:0], a...)
			b[random.Intn(len(b))] ^= 0x20
			check(string(a), string(b))
		}
	}
}

func TestEqualFoldASCIIWithoutAllocation(t *testing.T) {
	for _, pair := range [][2]string{{"Windows", "WINDOWS"}, {"file.TXT", "FILE.txt"}, {"中A", "中a"}, {"鸿草", "鸿蒙"}, {"\xffA", "\xffa"}} {
		var got bool
		allocations := testing.AllocsPerRun(1000, func() { got = EqualFoldASCII(pair[0], pair[1]) })
		if allocations != 0 {
			t.Errorf("比较 %q 与 %q：结果=%v，分配=%.0f，期望零分配", pair[0], pair[1], got, allocations)
		}
	}
}
