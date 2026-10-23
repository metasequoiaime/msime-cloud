package textutil

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIsASCII(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"", true}, {"Windows #123", true}, {"\x00\t\n\r\x7f", true},
		{"中文", false}, {"aé", false}, {"K", false}, {"a😀z", false},
		{"\x80", false}, {"a\xff", false}, {"\xe4\xb8", false},
	} {
		if got := IsASCII(tc.text); got != tc.want {
			t.Errorf("ASCII 检测 %q：结果=%v，期望=%v", tc.text, got, tc.want)
		}
	}
}

func TestIsASCIIByteBoundaries(t *testing.T) {
	for value := range 256 {
		for _, offset := range []int{0, 1, 7, 8, 15, 16, 31, 32, 63, 64, 127} {
			text := strings.Repeat("a", offset) + string([]byte{byte(value)}) + "z"
			if got, want := IsASCII(text), value < 128; got != want {
				t.Fatalf("字节=%02x，位置=%d：结果=%v，期望=%v", value, offset, got, want)
			}
		}
	}
}

func TestIsASCIIMatchesRuneScan(t *testing.T) {
	random := rand.New(rand.NewSource(20261024))
	for range 1000 {
		input := make([]byte, random.Intn(256))
		_, _ = random.Read(input)
		for _, asciiOnly := range []bool{false, true} {
			if asciiOnly {
				for i := range input {
					input[i] &= 0x7f
				}
			}
			text := string(input)
			want := strings.IndexFunc(text, func(r rune) bool { return r >= utf8.RuneSelf }) < 0
			if got := IsASCII(text); got != want {
				t.Fatalf("与 Unicode 扫描不同：输入=%q，结果=%v，期望=%v", text, got, want)
			}
		}
	}
}

func TestIsASCIIWithoutAllocation(t *testing.T) {
	for _, text := range []string{"", "Windows #123", strings.Repeat("a", 1024), "ASCII中文", "\xff"} {
		var got bool
		if allocations := testing.AllocsPerRun(1000, func() { got = IsASCII(text) }); allocations != 0 {
			t.Errorf("检测 %q：结果=%v，分配=%.0f，期望零分配", text, got, allocations)
		}
	}
}
