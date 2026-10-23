package textutil

import (
	"math"
	"strings"
	"testing"
)

func TestParsePrefixedID(t *testing.T) {
	for _, tc := range []struct {
		value  string
		prefix byte
		want   int64
	}{
		{"D1", 'D', 1},
		{"D42", 'D', 42},
		{"D9223372036854775807", 'D', math.MaxInt64},
		{"F1", 'F', 1},
		{"F42", 'F', 42},
		{"F9223372036854775807", 'F', math.MaxInt64},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got, ok := ParsePrefixedID(tc.value, tc.prefix); !ok || got != tc.want {
				t.Fatalf("有效编号 %q：结果=%d，成功=%v，期望=%d", tc.value, got, ok, tc.want)
			}
		})
	}
	for _, value := range []string{
		"", "D", "F1", "d1", "DD1", "1", "D0", "D00", "D01", "D+1", "D-1", "D-0",
		"D9223372036854775808", "D18446744073709551615", "D" + strings.Repeat("9", 128),
		" D1", "D 1", "D1 ", "D1\n", "D1\r", "D1\t", "D1\x00", "D1\xff",
		"D１", "D١", "D1\u00a0", "D1.0", "D1e2", "D0x10", "D1_0", "D1/close",
	} {
		t.Run(value, func(t *testing.T) {
			if got, ok := ParsePrefixedID(value, 'D'); ok || got != 0 {
				t.Fatalf("非法编号 %q：结果=%d，成功=%v", value, got, ok)
			}
		})
	}
}
