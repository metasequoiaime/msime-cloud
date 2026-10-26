package server

import (
	"strings"
	"testing"
)

func TestQQCommandPrefixBasicFoldMatchesLowercase(t *testing.T) {
	check := func(prefix string, words ...string) {
		t.Helper()
		lower := strings.ToLower(prefix)
		for _, word := range words {
			if got, want := qqCommandPrefixEqualFold(prefix, word), strings.HasPrefix(lower, word); got != want {
				t.Fatalf("命令前缀比较变化：输入=%q，命令=%q，结果=%t，期望=%t", prefix, word, got, want)
			}
		}
	}
	check("", "", "help")
	for r := rune(0); r < 128; r++ {
		prefix := string(r) + "Help"
		lower := strings.ToLower(string(r))
		check(prefix, "", string(r), lower, lower+"help", lower+"Help", "水")
	}
	for r := rune(0x4DFF); r <= 0xA000; r++ {
		prefix := string(r) + "Help"
		check(prefix, "", string(r), string(r)+"help", string(r)+"Help", "水")
	}
	for _, prefix := range []string{"提ISSUE：内容", "K", "İ", "Σ", "Ȿ", "Ⱥ", "ǅ", "𠀀HELP", "\ufffdHELP"} {
		check(prefix, "", "提issue", "提ISSUE", "k", "K", "i", "σ", "ȿ", "ⱥ", "ǆ", "𠀀help", "\ufffdhelp")
	}
}

func TestQQCommandPrefixBasicFoldKeepsInvalidUTF8(t *testing.T) {
	for _, tc := range []struct {
		prefix, word string
		want         bool
	}{
		{"\xff", "\ufffd", true},
		{"\xff", "\xff", false},
		{"\xffABC", "\ufffda", true},
		{"\xe4\xb8ABC", "\ufffd\ufffdabc", true},
		{"水\xe4\xb8ABC", "水\ufffd\ufffdabc", true},
		{"水\xffABC", "水\ufffdA", false},
	} {
		if got := qqCommandPrefixEqualFold(tc.prefix, tc.word); got != tc.want {
			t.Fatalf("无效 UTF-8 的前缀比较变化：输入=%q，命令=%q，结果=%t，期望=%t", tc.prefix, tc.word, got, tc.want)
		}
	}
}
