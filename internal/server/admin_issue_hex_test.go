package server

import (
	"strings"
	"testing"
)

func TestIssueVersionHexCompatibility(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", 64), strings.Repeat("A", 64), strings.Repeat("aB01", 16)} {
		if !issueValidVersion(value) {
			t.Fatalf("合法版本被拒绝：%q", value)
		}
	}
	for _, value := range []string{"", strings.Repeat("a", 32), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), " " + strings.Repeat("a", 63), strings.Repeat("a", 63) + "\x00"} {
		if issueValidVersion(value) {
			t.Fatalf("非法版本被接受：%q", value)
		}
	}
}
