package textutil

import (
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

func TestValidHex(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		size  int
		valid bool
	}{
		{"空编码", "", 0, true},
		{"数字", "0123456789", 5, true},
		{"小写", strings.Repeat("abcdef01", 4), 16, true},
		{"大写", strings.Repeat("ABCDEF01", 4), 16, true},
		{"混合大小写", strings.Repeat("aBcDeF01", 8), 32, true},
		{"缺失", "", 16, false},
		{"负长度", "", -1, false},
		{"超大长度", "", math.MaxInt, false},
		{"奇数长度", "abc", 1, false},
		{"过短", strings.Repeat("a", 30), 16, false},
		{"过长", strings.Repeat("a", 34), 16, false},
		{"前导空白", " " + strings.Repeat("a", 31), 16, false},
		{"尾随空白", strings.Repeat("a", 31) + "\n", 16, false},
		{"前缀", "0x" + strings.Repeat("a", 30), 16, false},
		{"非十六进制", strings.Repeat("a", 31) + "g", 16, false},
		{"控制字节", strings.Repeat("a", 31) + "\x00", 16, false},
		{"非ASCII", strings.Repeat("a", 30) + "é", 16, false},
		{"无效UTF8", strings.Repeat("a", 31) + "\xff", 16, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidHex(tc.value, tc.size); got != tc.valid {
				t.Fatalf("ValidHex(%q, %d)=%v，期望 %v", tc.value, tc.size, got, tc.valid)
			}
		})
	}
}

func TestValidHexMatchesStandardDecoder(t *testing.T) {
	for _, size := range []int{16, 32} {
		value := []byte(strings.Repeat("aB01", size/2))
		for _, at := range []int{0, len(value) / 2, len(value) - 1} {
			for b := 0; b < 256; b++ {
				value[at] = byte(b)
				text := string(value)
				decoded, err := hex.DecodeString(text)
				want := err == nil && len(decoded) == size
				if got := ValidHex(text, size); got != want {
					t.Fatalf("size=%d at=%d byte=%x：得到 %v，标准库得到 %v", size, at, b, got, want)
				}
			}
			value[at] = 'a'
		}
	}
}
