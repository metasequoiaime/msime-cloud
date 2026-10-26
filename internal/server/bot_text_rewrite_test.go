package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestQQTextRewriteMatches(t *testing.T) {
	for _, count := range []int{0, 1, 7, 8, 9, 16, 64} {
		for _, replacement := range []string{"", " ", "甲x乙"} {
			t.Run(fmt.Sprintf("%d/%q", count, replacement), func(t *testing.T) {
				text := "前" + strings.Repeat("x \xff 后 ", count) + "尾"
				search := 0
				next := func() (qqTextReplacement, bool) {
					index := strings.IndexByte(text[search:], 'x')
					if index < 0 {
						return qqTextReplacement{}, false
					}
					start := search + index
					search = start + 1
					return qqTextReplacement{start, search, replacement}, true
				}
				got := qqRewriteText(text, next, func(start int) { search = start })
				if want := strings.ReplaceAll(text, "x", replacement); got != want {
					t.Fatalf("重写结果变化：实际=%q，预期=%q", got, want)
				}
			})
		}
	}
}
