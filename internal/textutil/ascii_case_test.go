package textutil

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestHasUpperASCII(t *testing.T) {
	for _, tc := range []struct {
		text         string
		upper, ascii bool
	}{
		{"", false, true}, {"lowercase-12", false, true}, {"UPPERCASE", true, true},
		{"lowerZ", true, true}, {"A-lower", true, true}, {"\x00\t\n\x7f", false, true},
		{"中文", false, false}, {"A中文", false, false}, {"中文Z", false, false},
		{"Kind/bug", false, false}, {"İOS", false, false}, {"A\xff", false, false}, {"\xffA", false, false},
	} {
		if upper, ascii := HasUpperASCII(tc.text); upper != tc.upper || ascii != tc.ascii {
			t.Errorf("分类 %q = (%t, %t)，期望 (%t, %t)", tc.text, upper, ascii, tc.upper, tc.ascii)
		}
	}
}

func TestHasUpperASCIICompatibility(t *testing.T) {
	check := func(text string) {
		t.Helper()
		wantASCII := IsASCII(text)
		wantUpper := wantASCII && strings.ContainsAny(text, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		if upper, ascii := HasUpperASCII(text); upper != wantUpper || ascii != wantASCII {
			t.Fatalf("分类 %q = (%t, %t)，期望 (%t, %t)", text, upper, ascii, wantUpper, wantASCII)
		}
	}
	for value := range 256 {
		for _, offset := range []int{0, 1, 7, 8, 15, 16, 63, 64} {
			for _, prefix := range []string{"", "Z"} {
				check(prefix + strings.Repeat("a", offset) + string([]byte{byte(value)}) + "z")
			}
		}
	}
	random := rand.New(rand.NewPCG(43, 97))
	for range 2048 {
		input := make([]byte, random.IntN(256))
		for i := range input {
			input[i] = byte(random.Uint32())
		}
		check(string(input))
		for i := range input {
			input[i] &= 0x7f
		}
		check(string(input))
	}
}

func TestHasUpperASCIIWithoutAllocation(t *testing.T) {
	for _, text := range []string{"", "lowercase-12", "Mixed CASE", strings.Repeat("a", 1024), "A中文", "\xff"} {
		var upper, ascii bool
		if allocations := testing.AllocsPerRun(1000, func() { upper, ascii = HasUpperASCII(text) }); allocations != 0 {
			t.Errorf("分类 %q = (%t, %t)，分配 %.0f 次，期望零分配", text, upper, ascii, allocations)
		}
	}
}
