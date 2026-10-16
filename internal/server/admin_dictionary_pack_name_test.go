package server

import (
	"strings"
	"testing"
)

func TestDictionaryPackNameCompatibility(t *testing.T) {
	check := func(pack string) {
		t.Helper()
		name := "packs/" + pack + "/quanpin.txt"
		want := previousDictionaryPackNamePattern.MatchString(pack)
		if got := dictionaryEditablePath(name); got != want {
			t.Fatalf("包名 %q 的可编辑结果为 %t，期望 %t", pack, got, want)
		}
		if got := dictionaryTextPath(name); got != want {
			t.Fatalf("包名 %q 的可展示结果为 %t，期望 %t", pack, got, want)
		}
	}
	for _, pack := range []string{
		"", "a", "0", "z9_", "_a", "A", "a-b", "a.b", "a b", "a/b", "a\nb", "a\n",
		"中文", "é", "ａ", "a\xff", "a\x00", strings.Repeat("a", 64), strings.Repeat("a", 65),
		strings.Repeat("a", 63) + "_", strings.Repeat("a", 63) + "é", strings.Repeat("a", 1000),
	} {
		check(pack)
	}
	// 在短名称及长度边界的每个位置替换全部字节，覆盖首字符、控制字符和非法 UTF-8。
	for _, length := range []int{1, 2, 3, 63, 64, 65} {
		pack := []byte(strings.Repeat("a", length))
		for pos := range pack {
			for value := 0; value < 256; value++ {
				pack[pos] = byte(value)
				check(string(pack))
			}
			pack[pos] = 'a'
		}
	}
}
