package server

import (
	"regexp"
	"testing"
)

func TestQQRegexpSpaceMatchesRE2(t *testing.T) {
	pattern := regexp.MustCompile(`^\s$`)
	for value := range 256 {
		input := []byte{byte(value)}
		if got, want := qqRegexpSpace(input[0]), pattern.Match(input); got != want {
			t.Errorf("空白字节 %02x：实际=%v，正则结果=%v", value, got, want)
		}
	}
}
