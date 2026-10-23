package packageid

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, id := range []string{"a", "0", "shared.v1", "shared..v1", "shared-v1", "shared_v1", "con", "aux", "com1", "lpt9", "nul.txt", "null", "nul0", "fluent", "default", strings.Repeat("a", 64)} {
		if !Valid(id) {
			t.Errorf("误拒绝合法包 ID %q", id)
		}
	}
	for _, id := range []string{"", "nul", "shared.", "shared..", ".shared", "-shared", "_shared", "Shared", "aB", "a/b", "a\\b", "a b", "a\n", "a\x00", "a\x7f", "a\xff", "中文", "aé", strings.Repeat("a", 65)} {
		if Valid(id) {
			t.Errorf("接受了非法包 ID %q", id)
		}
	}
}

func TestValidMatchesPreviousPattern(t *testing.T) {
	// 以皮肤原有的正则规则为独立参照，覆盖字节位置、长度边界和随机输入。
	pattern := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	check := func(id string) {
		t.Helper()
		want := id != "nul" && !strings.HasSuffix(id, ".") && pattern.MatchString(id)
		if got := Valid(id); got != want {
			t.Fatalf("ID %q：结果=%v，期望=%v", id, got, want)
		}
	}
	for length := range 67 {
		check(strings.Repeat("a", length))
	}
	for offset := range 64 {
		for value := range 256 {
			input := []byte(strings.Repeat("a", 64))
			input[offset] = byte(value)
			check(string(input))
		}
	}
	random := rand.New(rand.NewSource(20261023))
	const alphabet = "abc012._-nulXYZ/\\ \n\xff"
	for range 2000 {
		input := make([]byte, random.Intn(80))
		for i := range input {
			input[i] = alphabet[random.Intn(len(alphabet))]
		}
		check(string(input))
	}
}

func TestValidWithoutAllocation(t *testing.T) {
	for _, id := range []string{"a", "shared.v1", strings.Repeat("a", 64), "nul", "shared.", "中文", "\xff"} {
		var got bool
		if allocations := testing.AllocsPerRun(100, func() { got = Valid(id) }); allocations != 0 {
			t.Errorf("ID %q：结果=%v，分配=%.0f，期望零分配", id, got, allocations)
		}
	}
}

func BenchmarkValid(b *testing.B) {
	for _, id := range []string{"community-clicks", strings.Repeat("a", 64), "shared.", "nul"} {
		b.Run(id, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Valid(id)
			}
		})
	}
}
