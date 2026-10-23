package server

import (
	"math/rand"
	"net/url"
	"strings"
	"testing"
)

func TestFormQueryEscapeMatchesNetURL(t *testing.T) {
	cases := []string{"", "word", " ", "word word", "azAZ09-_.~", "+%&=/?#", "词 😀\n\x00", "\xff\xfe\x00", strings.Repeat("中", 2730)}
	var allBytes [256]byte
	for i := range allBytes {
		allBytes[i] = byte(i)
		cases = append(cases, string(allBytes[i:i+1]))
	}
	cases = append(cases, string(allBytes[:]))
	for _, size := range []int{1021, 1022, 1023, 1024, 1025} {
		cases = append(cases, strings.Repeat("a", size)+" 中\xff", strings.Repeat("%", size), strings.Repeat(" ", size))
	}
	random := rand.New(rand.NewSource(20261021))
	for range 1000 {
		data := make([]byte, random.Intn(256))
		_, _ = random.Read(data)
		cases = append(cases, string(data))
	}
	for _, input := range cases {
		want := url.QueryEscape(input)
		if got := formQueryEscapeLen(input); got != len(want) {
			t.Fatalf("编码长度不一致：输入=%q，结果=%d，期望=%d", input, got, len(want))
		}
		var body strings.Builder
		body.Grow(len("prefix=&value=&next=") + len(want)*2)
		body.WriteString("prefix=&value=")
		appendFormQueryEscape(&body, input)
		body.WriteString("&next=")
		appendFormQueryEscape(&body, input)
		if got, expected := body.String(), "prefix=&value="+want+"&next="+want; got != expected {
			t.Fatalf("追加编码不一致：输入=%q，结果=%q，期望=%q", input, got, expected)
		}
	}
}
