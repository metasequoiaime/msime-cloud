package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func issueNormalizationServer(count int, title, repo string) *Server {
	s := &Server{}
	for i := range count {
		s.issues.items = append(s.issues.items, issueRow{Repo: repo, Number: i + 1, Title: title})
	}
	return s
}

func TestIssueSearchCaseNormalizationAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, title, repo string }{
		{"title", "Fix Candidate Window", "example/repository"},
		{"repo", "候选窗显示异常", "Example/Repository"},
		{"both", "Fix Candidate Window", "Example/Repository"},
		{"unicode", "候选窗 KİΣẞ 𐐀", "Example/Repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := issueNormalizationServer(20, tc.title, tc.repo)
			allocations := testing.AllocsPerRun(100, func() {
				if got := s.searchIssues("does-not-match"); got != nil {
					t.Fatalf("未命中搜索返回了结果：%#v", got)
				}
			})
			if allocations != 0 {
				t.Fatalf("未命中 issue 搜索分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func TestIssueSearchCaseNormalizationMatchesPrevious(t *testing.T) {
	s := &Server{}
	for i, title := range []string{
		"Fix Candidate Window", "候选窗 KİΣẞ 𐐀", "候选窗显示异常", "lowercase title",
		strings.Repeat("候选", 200) + "WINDOW KİΣ", "\xffWINDOW\xfe",
	} {
		s.issues.items = append(s.issues.items, issueRow{Repo: "Example/Repository", Number: i + 21, Title: title})
	}
	for _, query := range []string{
		"WINDOW", "candidate", "候选", "i", "K", "Σ", "σ", "ß", "ẞ", "𐐨", "�",
		"example/repository#21", "Repository#", "#21", "does-not-match", "  ", strings.Repeat("x", 1100),
	} {
		want := previousSearchIssues(s, query)
		got := s.searchIssues(query)
		if !slices.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("查询 %q 的结果或顺序变化：got=%#v want=%#v", query, got, want)
		}
	}
}

func BenchmarkIssueSearchCaseNormalization(b *testing.B) {
	for _, corpus := range []struct{ name, title, repo string }{
		{"ascii", "Fix Candidate Window", "Example/Repository"},
		{"lowercase", "候选窗显示异常", "example/repository"},
		{"unicode", "候选窗 KİΣẞ 𐐀", "Example/Repository"},
	} {
		for _, count := range []int{20, 1000} {
			s := issueNormalizationServer(count, corpus.title, corpus.repo)
			for _, tc := range []struct {
				name string
				fn   func(*Server, string) []account.AdminSearchHit
			}{
				{"previous", previousIssueSearchNormalization},
				{"current", (*Server).searchIssues},
			} {
				b.Run(corpus.name+"/"+strconv.Itoa(count)+"/"+tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if hits := tc.fn(s, "does-not-match"); hits != nil {
							b.Fatal("未命中搜索返回了结果")
						}
					}
				})
			}
		}
	}
}

func previousIssueSearchNormalization(s *Server, q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	number := strings.TrimPrefix(q, "#")
	m := s.issueMemory()
	m.mu.Lock()
	items := m.items
	m.mu.Unlock()
	var matched [8]account.AdminSearchHit
	count := 0
	for _, row := range items {
		n := strconv.Itoa(row.Number)
		if number != n && !strings.Contains(strings.ToLower(row.Title), q) && !strings.Contains(strings.ToLower(row.Repo+"#"+n), q) {
			continue
		}
		matched[count] = account.AdminSearchHit{Kind: "issue", ID: issueRef{Repo: row.Repo, N: row.Number}.key(), Title: "#" + n + " " + row.Title, Where: "问题分诊", Target: "issues"}
		count++
		if count == len(matched) {
			break
		}
	}
	if count == 0 {
		return nil
	}
	return slices.Clone(matched[:count])
}
