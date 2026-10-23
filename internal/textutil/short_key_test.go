package textutil

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestValidShortKey(t *testing.T) {
	for _, value := range []string{"a", "0", "database", "cn-mirror", "niutrans_voice", "a_-", "nul", strings.Repeat("a", 32)} {
		if !ValidShortKey(value) {
			t.Errorf("误拒绝短键 %q", value)
		}
	}
	for _, value := range []string{"", "_a", "-a", "A", "aB", "a.b", "a+b", "a/b", "a\\b", "a b", "a\n", "a\r", "a\x00", "a\x7f", "a\xff", "中文", "aé", "K", strings.Repeat("a", 33)} {
		if ValidShortKey(value) {
			t.Errorf("接受了非法短键 %q", value)
		}
	}
}

func TestValidShortKeyMatchesPreviousPattern(t *testing.T) {
	// 旧正则作为独立参照，覆盖长度边界、每个位置的全部字节及随机组合。
	pattern := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	check := func(value string) {
		t.Helper()
		if got, want := ValidShortKey(value), pattern.MatchString(value); got != want {
			t.Fatalf("短键 %q：结果=%v，期望=%v", value, got, want)
		}
	}
	for length := range 35 {
		check(strings.Repeat("a", length))
	}
	for offset := range 32 {
		for value := range 256 {
			input := []byte(strings.Repeat("a", 32))
			input[offset] = byte(value)
			check(string(input))
		}
	}
	random := rand.New(rand.NewSource(20261023))
	const alphabet = "az019_-AZ.+/\\ \n\x00\xff"
	for range 2000 {
		input := make([]byte, random.Intn(40))
		for i := range input {
			input[i] = alphabet[random.Intn(len(alphabet))]
		}
		check(string(input))
	}
}

func TestValidShortKeyWithoutAllocation(t *testing.T) {
	for _, value := range []string{"", "database", "cn-mirror", strings.Repeat("a", 32), "a\xff"} {
		var got bool
		if allocations := testing.AllocsPerRun(100, func() { got = ValidShortKey(value) }); allocations != 0 {
			t.Errorf("短键 %q：结果=%v，分配=%.0f，期望零分配", value, got, allocations)
		}
	}
}
