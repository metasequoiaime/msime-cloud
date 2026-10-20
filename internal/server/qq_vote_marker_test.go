package server

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var previousQQVotePattern = regexp.MustCompile(`<!-- qq-votes:([0-9]{1,7}) -->`)

func previousQQVoteCount(body string) (int, bool) {
	match := previousQQVotePattern.FindStringSubmatch(body)
	if match == nil {
		return 0, false
	}
	n, err := strconv.Atoi(match[1])
	return n, err == nil
}

func TestQQVoteCountMatchesPattern(t *testing.T) {
	cases := []string{
		"", "普通评论", "<!-- qq-votes:1 -->", "前文 <!-- qq-votes:0000042 --> 后文",
		"<!-- qq-votes:1234567 -->", "<!-- qq-votes:12345678 -->", "<!-- qq-votes: -->",
		"<!-- qq-votes:1--> ", "<!-- qq-votes:1  -->", "<!-- qq-votes:1 --> <!-- qq-votes:2 -->",
		"<!-- qq-votes:1 -->x<!-- qq-votes:2 -->", strings.Repeat("普通评论。", 1000) + "<!-- qq-votes:9 -->",
	}
	random := rand.New(rand.NewSource(20261020))
	units := []string{"", "普通", "<!-- qq-votes:", " -->", "0", "1", "7", " ", "x", "\xff"}
	for range 1000 {
		var text strings.Builder
		for range random.Intn(80) {
			text.WriteString(units[random.Intn(len(units))])
		}
		cases = append(cases, text.String())
	}
	for _, body := range cases {
		got, gotOK := qqVoteCount(body)
		want, wantOK := previousQQVoteCount(body)
		if got != want || gotOK != wantOK {
			t.Fatalf("投票标记解析变化：输入=%q，结果=(%d,%v)，期望=(%d,%v)", body, got, gotOK, want, wantOK)
		}
	}
}

func TestQQVoteCountAvoidsAllocation(t *testing.T) {
	body := strings.Repeat("普通评论。", 1000) + "<!-- qq-votes:1234567 -->"
	if allocations := testing.AllocsPerRun(100, func() {
		if n, ok := qqVoteCount(body); !ok || n != 1234567 {
			t.Fatalf("投票标记未解析：%d %v", n, ok)
		}
	}); allocations != 0 {
		t.Fatalf("投票标记解析分配 %.0f 次，期望零分配", allocations)
	}
}

func BenchmarkQQVoteCount(b *testing.B) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing", strings.Repeat("普通评论。", 1000)},
		{"tail", strings.Repeat("普通评论。", 1000) + "<!-- qq-votes:1234567 -->"},
		{"early", "<!-- qq-votes:42 -->" + strings.Repeat("普通评论。", 1000)},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string) (int, bool)
		}{
			{"previous", previousQQVoteCount}, {"current", qqVoteCount},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_, _ = impl.fn(tc.body)
				}
			})
		}
	}
}
