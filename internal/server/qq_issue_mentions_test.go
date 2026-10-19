package server

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 固定全文匹配方式，对照引用顺序、去重以及按原始匹配次数决定是否附正文的规则。
var previousQQIssueMentionPattern = regexp.MustCompile(`(?:([A-Za-z0-9._/-]{1,100})\s*)?#([0-9]{1,7})\b`)

func (s *Server) previousQQMentionedIssues(ctx context.Context, question string) string {
	var lines []string
	seen := map[string]bool{}
	question = qqIssueLinkPattern.ReplaceAllString(qqFoldWidth(question), " $1 #$2 ")
	mentions := previousQQIssueMentionPattern.FindAllStringSubmatch(question, -1)
	for _, m := range mentions {
		key := strings.ToLower(m[1]) + "#" + m[2]
		if seen[key] {
			continue
		}
		if seen[key] = true; len(seen) > qqMentionedIssueLimit {
			break
		}
		name := m[1]
		if !slices.ContainsFunc(s.qqGitHubRepos(), func(r string) bool {
			_, short, _ := strings.Cut(r, "/")
			return strings.EqualFold(short, name) || strings.EqualFold(r, name)
		}) {
			name = ""
		}
		lines = append(lines, s.qqIssueStatus(ctx, name, m[2], qqMaintainerRunes))
		if len(mentions) <= 2 {
			if body := s.qqIssueBodyText(ctx, name, m[2]); body != "" {
				lines = append(lines, "正文："+body)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func qqIssueMentionTestServer(t *testing.T) *Server {
	t.Helper()
	s, fake, _ := qqServer(t)
	s.config.Admin.GitHub.Platforms = nil
	fake.issues = map[string]string{}
	for number := 1; number <= 9; number++ {
		fake.issues[fmt.Sprintf("o/r/issues/%d", number)] = fmt.Sprintf(`{"number":%d,"title":"合成问题 %d","state":"open","body":"合成正文 %d。","author_association":"MEMBER","labels":[],"updated_at":"2026-10-11T00:00:00Z"}`, number, number, number)
	}
	return s
}

func TestQQIssueMentionTraversalCompatibility(t *testing.T) {
	s := qqIssueMentionTestServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		question string
		bodies   int
	}{
		{"", 0},
		{"普通问题", 0},
		{"#1", 1},
		{"r #1，R #1", 1},
		{"#1 #1 #1", 0},
		{"r #1，o/r #1", 2},
		{"Windows #1，mac #1", 2},
		{"#1 #2", 2},
		{"#1 #2 #3 #4", 0},
		{"＃１ ＃２", 2},
		{"https://github.com/metasequoiaime/r/issues/1", 1},
		{"http://github.com/metasequoiaime/r/pull/2 #2", 2},
		{"#12345678 #1x #2_ #3ſ #4K", 2},
		{"\xffr #1\xfeR #2\xe4\xb8", 2},
		{strings.Repeat("#1 ", 8) + "#2 #3", 0},
		{strings.Repeat("r #1，", 17) + "r #2，r #3", 0},
		{strings.Repeat("R #1，r #1，", 4096) + "o/r #2，r #3", 0},
	} {
		got, want := s.qqMentionedIssues(ctx, tc.question), s.previousQQMentionedIssues(ctx, tc.question)
		if got != want || strings.Count(got, "正文：") != tc.bodies {
			t.Fatalf("引用资料=%q，旧实现=%q；正文数量期望 %d；输入=%q", got, want, tc.bodies, tc.question)
		}
	}
	random := rand.New(rand.NewSource(79))
	units := []string{"r #1", "R #1", "#2", "o/r #3", "Windows #4", "#12345678", "#1x", "#2_", "＃１", "https://github.com/metasequoiaime/r/issues/2", "http://github.com/metasequoiaime/r/pull/3", "r", " ", "\n", "\t", "。", "，", "\xff", "\xe4\xb8", "😀"}
	for range 1000 {
		var question strings.Builder
		for range random.Intn(32) {
			question.WriteString(units[random.Intn(len(units))])
		}
		text := question.String()
		if got, want := s.qqMentionedIssues(ctx, text), s.previousQQMentionedIssues(ctx, text); got != want {
			t.Fatalf("引用遍历不兼容：结果=%q，旧实现=%q；输入=%q", got, want, text)
		}
	}
}

func TestQQIssueMentionAllocationBudget(t *testing.T) {
	s := qqIssueMentionTestServer(t)
	ctx := context.Background()
	prefix := "r #1，r #2，r #3，"
	want := s.qqMentionedIssues(ctx, prefix)
	question := prefix + strings.Repeat("r #4，", 4096)
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got = s.qqMentionedIssues(ctx, question)
		}
	})
	if got != want {
		t.Fatalf("超过三个引用时资料变化：结果=%q，期望 %q", got, want)
	}
	t.Logf("首批已凑齐三个引用：%s，%s", result.String(), result.MemString())
	// 竞态检测会随机丢弃正则对象池，分配预算在普通模式检查。
	if !raceEnabled && result.AllocedBytesPerOp() > 128<<10 {
		t.Fatalf("首批已凑齐三个引用时分配 %d 字节，超过 128 KiB 预算", result.AllocedBytesPerOp())
	}
}
