package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func issueSearchCapacityServer(count int) *Server {
	s := &Server{}
	for i := range count {
		s.issues.items = append(s.issues.items, issueRow{Repo: "example/repo", Number: i + 1, Title: "候选窗显示异常"})
	}
	return s
}

func TestIssueSearchResultAllocationBudget(t *testing.T) {
	for _, count := range []int{0, 1, 5, 8, 20} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := issueSearchCapacityServer(count)
			want := previousSearchIssues(s, "候选窗")
			var got []account.AdminSearchHit
			allocations := testing.AllocsPerRun(100, func() { got = s.searchIssues("候选窗") })
			if !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("issue 搜索结果或顺序变化：got=%#v want=%#v", got, want)
			}
			budget := 0.0
			if len(want) > 0 {
				// 每条命中的结果标识和标题各需一次字符串分配，结果切片最多分配一次。
				budget = float64(2*len(want) + 1)
			}
			if allocations > budget {
				t.Fatalf("issue 搜索分配 %.0f 次，期望最多 %.0f 次", allocations, budget)
			}
			if len(got) > 0 {
				got[0].Title = "调用方修改结果"
				if again := s.searchIssues("候选窗"); !slices.Equal(again, want) {
					t.Fatalf("搜索结果修改影响后续查询：%#v", again)
				}
			}
		})
	}
}

func TestIssueSearchUnmatchedResultAllocations(t *testing.T) {
	s := issueSearchCapacityServer(20)
	for _, query := range []string{"", "  ", "不存在的标题"} {
		t.Run(query, func(t *testing.T) {
			allocations := testing.AllocsPerRun(100, func() {
				if got := s.searchIssues(query); got != nil {
					t.Fatalf("未命中搜索返回了结果：%#v", got)
				}
			})
			if allocations != 0 {
				t.Fatalf("未命中 issue 搜索分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func BenchmarkIssueSearchResultCapacity(b *testing.B) {
	for _, count := range []int{0, 1, 5, 8, 20} {
		s := issueSearchCapacityServer(count)
		for _, tc := range []struct {
			name string
			fn   func(*Server, string) []account.AdminSearchHit
		}{
			{"previous", previousSearchIssues},
			{"current", (*Server).searchIssues},
		} {
			b.Run(strconv.Itoa(count)+"/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := tc.fn(s, "候选窗"); len(got) != min(count, 8) {
						b.Fatal("命中 issue 结果数量变化")
					}
				}
			})
		}
	}
}

func previousSearchIssues(s *Server, q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	number := strings.TrimPrefix(q, "#")
	m := s.issueMemory()
	m.mu.Lock()
	items := m.items
	m.mu.Unlock()
	var hits []account.AdminSearchHit
	for _, row := range items {
		n := strconv.Itoa(row.Number)
		if number != n && !strings.Contains(strings.ToLower(row.Title), q) && !strings.Contains(strings.ToLower(row.Repo+"#"+n), q) {
			continue
		}
		hits = append(hits, account.AdminSearchHit{Kind: "issue", ID: issueRef{Repo: row.Repo, N: row.Number}.key(), Title: "#" + n + " " + row.Title, Where: "问题分诊", Target: "issues"})
		if len(hits) == 8 {
			break
		}
	}
	return hits
}
