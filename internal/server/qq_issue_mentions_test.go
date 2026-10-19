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

func checkQQIssueMentionIndexes(t *testing.T, question string) {
	t.Helper()
	for _, limit := range []int{-1, 0, 1, 2, 8} {
		got := qqFindIssueMentions(question, limit)
		want := previousQQIssueMentionPattern.FindAllStringSubmatchIndex(question, limit)
		if !slices.EqualFunc(got, want, func(a, b []int) bool { return slices.Equal(a, b) }) {
			t.Fatalf("引用捕获下标变化：limit=%d，结果=%v，旧实现=%v；输入=%q", limit, got, want, question)
		}
	}
	var batched [][]int
	for offset := 0; offset < len(question); {
		batch := qqFindIssueMentions(question[offset:], 8)
		if len(batch) == 0 {
			break
		}
		consumed := batch[len(batch)-1][1]
		for _, match := range batch {
			for i, index := range match {
				if index >= 0 {
					match[i] += offset
				}
			}
			batched = append(batched, match)
		}
		offset += consumed
	}
	want := previousQQIssueMentionPattern.FindAllStringSubmatchIndex(question, -1)
	if !slices.EqualFunc(batched, want, func(a, b []int) bool { return slices.Equal(a, b) }) {
		t.Fatalf("分批引用捕获变化：结果=%v，旧实现=%v；输入=%q", batched, want, question)
	}
}

func TestQQIssueMentionScanIndexesCompatibility(t *testing.T) {
	for _, text := range []string{
		"", "普通正文", "r #1 #2", "#1 #2 #3 #4 #5 #6 #7 #8 #9 #1",
		"#1x #2_ #12345678 #3ſ #4K", "r\v#1 r\f#2 r\u00a0#3",
		strings.Repeat("a", 99) + "#1", strings.Repeat("a", 100) + "#1", strings.Repeat("a", 101) + "#1",
		"r" + strings.Repeat(" \n\r\t\f", 4096) + "#1",
		strings.Repeat("r #1 ", 17) + "#2",
	} {
		checkQQIssueMentionIndexes(t, text)
	}
	for c := range 256 {
		checkQQIssueMentionIndexes(t, "r #1234567"+string(byte(c))+"#2")
		checkQQIssueMentionIndexes(t, "r"+string(byte(c))+"#1")
	}
	random := rand.New(rand.NewSource(89))
	units := []string{"#", "1", "1234567", "12345678", "a", "_", "o/r", ".-", "ſ", "K", "😀", "\v", "\f", "\t", "\r", "\n", " ", "\u00a0", "\u2003", "\xff", strings.Repeat("r", 101)}
	for range 2000 {
		var text strings.Builder
		for range random.Intn(32) {
			text.WriteString(units[random.Intn(len(units))])
		}
		checkQQIssueMentionIndexes(t, text.String())
		data := make([]byte, random.Intn(256))
		_, _ = random.Read(data)
		checkQQIssueMentionIndexes(t, string(data))
	}
}

func FuzzQQIssueMentionScanIndexes(f *testing.F) {
	for _, text := range []string{"", "r #1 #2", "#1x #2_ #12345678 #3ſ #4K", "r\v#1 r\f#2", "\xffr #1\xfe", strings.Repeat("a", 101) + " #1", strings.Repeat("r #1 ", 17) + "#2"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, question string) {
		if len(question) > 4096 {
			t.Skip()
		}
		checkQQIssueMentionIndexes(t, question)
	})
}

func BenchmarkQQIssueMentionScan(b *testing.B) {
	for _, tc := range []struct{ name, question string }{
		{"short", "#1"},
		{"repository", "o/r #1"},
		{"dense", strings.Repeat("r #1 ", 512)},
		{"sparse", strings.Repeat("ordinary question ", 111) + "#1"},
	} {
		for _, implementation := range []struct {
			name string
			find func(string, int) [][]int
		}{
			{"regexp", previousQQIssueMentionPattern.FindAllStringSubmatchIndex},
			{"scan", qqFindIssueMentions},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.find(tc.question, 8)
				}
			})
		}
	}
}

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

func TestQQRewriteIssueLinksCompatibility(t *testing.T) {
	link := "https://github.com/metasequoiaime/r/issues/1"
	units := []string{
		link,
		"http://github.com/metasequoiaime/a._-/pull/12345678",
		"https://github.com/metasequoiaime/" + strings.Repeat("r", 100) + "/issues/1",
		"https://github.com/metasequoiaime/" + strings.Repeat("r", 101) + "/issues/1",
		"https://github.com/example/r/issues/1",
		"https://github.com/metasequoiaime/r/issues/",
		"https://github.com/metasequoiaime/r/issues/０",
		"正文", " ", "\n", "\xff", "\xe4\xb8", "😀",
	}
	check := func(question string) {
		t.Helper()
		want := qqIssueLinkPattern.ReplaceAllString(question, " $1 #$2 ")
		if got := qqRewriteIssueLinks(question); got != want {
			t.Fatalf("链接替换=%q，旧实现=%q；输入=%q", got, want, question)
		}
	}
	for _, question := range append(units, "", link+link, strings.Repeat(link+" ", 512)) {
		check(question)
	}
	random := rand.New(rand.NewSource(83))
	for range 1000 {
		var question strings.Builder
		for range random.Intn(32) {
			question.WriteString(units[random.Intn(len(units))])
		}
		check(question.String())
	}
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
		{"ｈｔｔｐｓ://github.com/metasequoiaime/ｒ/issues/１", 1},
		{"https://github.com/metasequoiaime/r/issues/1https://github.com/metasequoiaime/r/pull/2", 2},
		{"https://github.com/example/r/issues/1", 0},
		{"https://github.com/metasequoiaime/r/issues/", 0},
		{"https://github.com/metasequoiaime/" + strings.Repeat("r", 101) + "/issues/1", 0},
		{"\xffhttps://github.com/metasequoiaime/r/issues/1\xfe", 1},
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

func TestQQIssueMentionNoLinkAllocationBudget(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	for _, tc := range []struct{ name, question string }{
		{"plain_ascii", strings.Repeat("ordinary question ", 4096)},
		{"plain_chinese", strings.Repeat("普通输入法问题。", 4096)},
		{"other_organization", strings.Repeat("https://github.com/example/r/issues/1 ", 4096)},
		{"incomplete_link", strings.Repeat("https://github.com/metasequoiaime/r/issues/ ", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				// 在计时前预热同尺寸输入的正则状态，避免首次分配被低迭代次数放大。
				got = s.qqMentionedIssues(ctx, tc.question)
				b.ReportAllocs()
				for b.Loop() {
					got = s.qqMentionedIssues(ctx, tc.question)
				}
			})
			if got != "" {
				t.Fatalf("没有 issue 引用时资料应为空，结果=%q", got)
			}
			t.Logf("%s：%s，%s", tc.name, result.String(), result.MemString())
			// 竞态检测会随机丢弃正则对象池，沿用引用遍历测试的做法，在普通模式检查分配预算。
			if !raceEnabled && result.AllocedBytesPerOp() > 1024 {
				t.Fatalf("无链接替换时分配 %d 字节，超过 1024 字节预算", result.AllocedBytesPerOp())
			}
		})
	}
}

func TestQQIssueMentionNoHashScanBudget(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	for _, tc := range []struct{ name, question string }{
		{"message_limit", strings.Repeat("ordinary question ", qqBotMaxRunes/len("ordinary question "))},
		{"plain_ascii", strings.Repeat("ordinary question ", 4096)},
		{"other_organization", strings.Repeat("https://github.com/example/r/issues/1 ", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 单独执行旧的引用正则作为保守基线，不计入全半角折叠及链接替换开销。
			var matches [][]int
			baseline := testing.Benchmark(func(b *testing.B) {
				matches = previousQQIssueMentionPattern.FindAllStringSubmatchIndex(tc.question, 8)
				for b.Loop() {
					matches = previousQQIssueMentionPattern.FindAllStringSubmatchIndex(tc.question, 8)
				}
			})
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				got = s.qqMentionedIssues(ctx, tc.question)
				for b.Loop() {
					got = s.qqMentionedIssues(ctx, tc.question)
				}
			})
			if len(matches) != 0 || got != "" {
				t.Fatalf("无引用输入的匹配=%v，资料=%q", matches, got)
			}
			t.Logf("引用正则基线：%s；完整提问处理：%s", baseline, result)
			// 竞态插桩会改变各处理阶段的相对耗时，只在普通模式检查扫描预算。
			if !raceEnabled && result.NsPerOp() >= baseline.NsPerOp()/2 {
				t.Fatalf("无井号输入仍承担引用正则扫描：基线=%s，实际=%s", baseline, result)
			}
		})
	}
}

func TestQQIssueMentionSparseScanBudget(t *testing.T) {
	s := qqIssueMentionTestServer(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, question string }{
		{"message_limit", strings.Repeat("ordinary question ", (qqBotMaxRunes-3)/len("ordinary question ")) + " #1"},
		{"long_ascii", strings.Repeat("ordinary question ", 4096) + " #1"},
		{"long_whitespace", "r" + strings.Repeat(" \t\n\r\f", 4096) + "#1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := s.previousQQMentionedIssues(ctx, tc.question)
			baseline := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = previousQQIssueMentionPattern.FindAllStringSubmatchIndex(tc.question, 8)
				}
			})
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				got = s.qqMentionedIssues(ctx, tc.question)
				for b.Loop() {
					got = s.qqMentionedIssues(ctx, tc.question)
				}
			})
			if got != want || strings.Count(got, "正文：") != 1 {
				t.Fatalf("稀疏引用资料变化：结果=%q，旧实现=%q", got, want)
			}
			t.Logf("引用正则基线：%s；完整提问处理：%s", baseline, result)
			// 竞态插桩改变各阶段相对耗时，只在普通模式检查扫描预算。
			if !raceEnabled && result.NsPerOp() >= baseline.NsPerOp()/2 {
				t.Fatalf("稀疏引用仍承担全文正则扫描：基线=%s，实际=%s", baseline, result)
			}
		})
	}
}
