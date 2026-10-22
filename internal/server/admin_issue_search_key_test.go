package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func issueKeySearchServer(count int, repo string) *Server {
	s := &Server{}
	for i := range count {
		s.issues.items = append(s.issues.items, issueRow{Repo: repo, Number: 1000 + i, Title: "Candidate Window"})
	}
	return s
}

func TestIssueSearchUnqualifiedKeyAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, repo string }{
		{"ascii", "Example/" + strings.Repeat("Repository", 8)},
		{"unicode", "例子/" + strings.Repeat("仓库", 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := issueKeySearchServer(1000, tc.repo)
			allocations := testing.AllocsPerRun(100, func() {
				if got := s.searchIssues("missing"); got != nil {
					t.Fatalf("未命中搜索返回了结果：%#v", got)
				}
			})
			if allocations != 0 {
				t.Fatalf("长仓库名未命中搜索分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func TestIssueSearchKeyPartsMatchPrevious(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, repo := range []string{
		"Example/Repository", "Example/" + strings.Repeat("Repository", 8),
		"例子/K仓库" + strings.Repeat("仓库", 16), "Example/Repo#fragment", "",
	} {
		s := &Server{}
		for _, number := range []int{-1000, 0, 99, 100, 1000, 1001, maxInt, -maxInt - 1} {
			s.issues.items = append(s.issues.items, issueRow{Repo: repo, Number: number, Title: "Candidate Window"})
		}
		for _, query := range []string{
			"missing", "example", "repository", "window", "100", "0", "-1", "K", "仓库",
			"#1000", "#100", "#-1000", "repository#1000", "Y#1000", "库#1000", "Repo#frag", "fragment#", "#", "##", "  ",
		} {
			want := previousSearchIssues(s, query)
			got := s.searchIssues(query)
			if !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("仓库 %q、查询 %q 的结果或顺序变化：got=%#v want=%#v", repo, query, got, want)
			}
		}
	}
}

func BenchmarkIssueSearchKeyConstruction(b *testing.B) {
	for _, corpus := range []struct{ name, repo string }{
		{"short", "Example/Repository"},
		{"long", "Example/" + strings.Repeat("Repository", 8)},
	} {
		s := issueKeySearchServer(1000, corpus.repo)
		for _, query := range []string{"missing", "repository", "100", "#100", "y#1000"} {
			want := previousIssueSearchKeyConstruction(s, query)
			for _, tc := range []struct {
				name string
				fn   func(*Server, string) []account.AdminSearchHit
			}{
				{"previous", previousIssueSearchKeyConstruction},
				{"current", (*Server).searchIssues},
			} {
				b.Run(corpus.name+"/"+query+"/"+tc.name, func(b *testing.B) {
					if got := tc.fn(s, query); !slices.Equal(got, want) || (got == nil) != (want == nil) {
						b.Fatal("仓库键搜索结果变化")
					}
					b.ReportAllocs()
					for b.Loop() {
						if got := tc.fn(s, query); len(got) != len(want) {
							b.Fatal("仓库键搜索结果数量变化")
						}
					}
				})
			}
		}
	}
}

func previousIssueSearchKeyConstruction(s *Server, q string) []account.AdminSearchHit {
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
	var numberBuffer [20]byte
	count := 0
	for _, row := range items {
		var n string
		if 0 <= row.Number && row.Number < 100 {
			n = strconv.Itoa(row.Number)
		} else {
			n = string(strconv.AppendInt(numberBuffer[:0], int64(row.Number), 10))
		}
		if number != n && !dictionaryLineContainsLower(row.Title, q) && !dictionaryLineContainsLower(row.Repo+"#"+n, q) {
			continue
		}
		matched[count] = account.AdminSearchHit{Kind: "issue", ID: row.Repo + "#" + n, Title: "#" + n + " " + row.Title, Where: "问题分诊", Target: "issues"}
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
