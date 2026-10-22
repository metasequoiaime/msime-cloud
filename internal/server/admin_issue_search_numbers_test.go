package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func issueNumberSearchServer(count, start int) *Server {
	s := &Server{}
	for i := range count {
		s.issues.items = append(s.issues.items, issueRow{Repo: "example/repository", Number: start + i, Title: "Candidate Window"})
	}
	return s
}

func TestIssueSearchNumberFormattingAllocations(t *testing.T) {
	for _, start := range []int{100, 1000, -1000} {
		t.Run(strconv.Itoa(start), func(t *testing.T) {
			s := issueNumberSearchServer(1000, start)
			allocations := testing.AllocsPerRun(100, func() {
				if got := s.searchIssues("missing"); got != nil {
					t.Fatalf("未命中搜索返回了结果：%#v", got)
				}
			})
			if allocations != 0 {
				t.Fatalf("未命中大编号搜索分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func TestIssueSearchNumberBoundaries(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	s := &Server{}
	for _, number := range []int{maxInt, -maxInt - 1, -1000, -1, 0, 1000, 1001} {
		s.issues.items = append(s.issues.items, issueRow{Repo: "o/r", Number: number, Title: "Candidate Window"})
	}
	queries := []string{"window", "missing", "100", "  "}
	for _, row := range s.issues.items {
		n := strconv.Itoa(row.Number)
		queries = append(queries, n, "#"+n, "o/r#"+n)
	}
	for _, query := range queries {
		want := previousSearchIssues(s, query)
		got := s.searchIssues(query)
		if !slices.Equal(got, want) || (got == nil) != (want == nil) {
			t.Fatalf("编号查询 %q 的结果或顺序变化：got=%#v want=%#v", query, got, want)
		}
	}
	allocations := testing.AllocsPerRun(100, func() { _ = s.searchIssues("missing") })
	if allocations != 0 {
		t.Fatalf("整数边界的未命中搜索分配 %.0f 次，期望零分配", allocations)
	}
}

func TestIssueSearchNumberOwnership(t *testing.T) {
	s := issueNumberSearchServer(8, 1000)
	want := previousSearchIssues(s, "window")
	got := s.searchIssues("window")
	for i := range s.issues.items {
		s.issues.items[i].Number = 9000 + i
	}
	_ = s.searchIssues("window")
	if !slices.Equal(got, want) {
		t.Fatalf("后续编号格式化改写了先前结果：got=%#v want=%#v", got, want)
	}
}

func BenchmarkIssueSearchNumberFormatting(b *testing.B) {
	for _, corpus := range []struct {
		name         string
		count, start int
	}{
		{"small", 20, 1},
		{"mixed", 1000, 1},
		{"large", 1000, 1000},
	} {
		s := issueNumberSearchServer(corpus.count, corpus.start)
		for _, query := range []string{"missing", "#1000", "window"} {
			want := previousIssueSearchNumbers(s, query)
			for _, tc := range []struct {
				name string
				fn   func(*Server, string) []account.AdminSearchHit
			}{
				{"previous", previousIssueSearchNumbers},
				{"current", (*Server).searchIssues},
			} {
				b.Run(corpus.name+"/"+query+"/"+tc.name, func(b *testing.B) {
					if got := tc.fn(s, query); !slices.Equal(got, want) || (got == nil) != (want == nil) {
						b.Fatal("编号搜索结果变化")
					}
					b.ReportAllocs()
					for b.Loop() {
						if got := tc.fn(s, query); len(got) != len(want) {
							b.Fatal("编号搜索结果数量变化")
						}
					}
				})
			}
		}
	}
}

func previousIssueSearchNumbers(s *Server, q string) []account.AdminSearchHit {
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
		if number != n && !dictionaryLineContainsLower(row.Title, q) && !dictionaryLineContainsLower(row.Repo+"#"+n, q) {
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
