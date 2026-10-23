package textutil

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestValidClientLabel(t *testing.T) {
	for _, value := range []string{"a", "Z", "0", "zh-CN", "Android_15", "1.4.0+build", "a._+-", "nul", "CON", strings.Repeat("A", 64)} {
		if !ValidClientLabel(value) {
			t.Errorf("误拒绝客户端标识 %q", value)
		}
	}
	for _, value := range []string{"", ".a", "_a", "+a", "-a", "a/b", "a\\b", "a:b", "a b", "a\t", "a\n", "a\r\n", "a\x00", "a\x7f", "a\x80", "a\xff", "a\xc3", "中文", "aé", "K", strings.Repeat("A", 65)} {
		if ValidClientLabel(value) {
			t.Errorf("接受了非法客户端标识 %q", value)
		}
	}
}

func TestValidClientLabelMatchesPreviousPattern(t *testing.T) {
	// 原有正则作为独立参照，核对长度边界、每个位置的全部字节和随机输入。
	pattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	check := func(value string) {
		t.Helper()
		if got, want := ValidClientLabel(value), pattern.MatchString(value); got != want {
			t.Fatalf("标识 %q：结果=%v，期望=%v", value, got, want)
		}
	}
	for length := range 67 {
		check(strings.Repeat("A", length))
	}
	for offset := range 64 {
		for value := range 256 {
			input := []byte(strings.Repeat("A", 64))
			input[offset] = byte(value)
			check(string(input))
		}
	}
	random := rand.New(rand.NewSource(20261023))
	const alphabet = "aAzZ019._+-/\\ \x00\n\xff"
	for range 2000 {
		input := make([]byte, random.Intn(80))
		for i := range input {
			input[i] = alphabet[random.Intn(len(alphabet))]
		}
		check(string(input))
	}
}

func TestValidClientLabelWithoutAllocation(t *testing.T) {
	for _, value := range []string{"", "zh-CN", "1.4.0+build", strings.Repeat("A", 64), "a\xff"} {
		var got bool
		if allocations := testing.AllocsPerRun(100, func() { got = ValidClientLabel(value) }); allocations != 0 {
			t.Errorf("标识 %q：结果=%v，分配=%.0f，期望零分配", value, got, allocations)
		}
	}
}
