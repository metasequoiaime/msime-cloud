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
		for _, query := range []string{"missing", "repository", "100", "#100", "y#1000", "missing#1000"} {
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
	queryHasHash := strings.ContainsRune(q, '#')
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
		if number != n && !dictionaryLineContainsLower(row.Title, q) {
			if queryHasHash {
				if !dictionaryLineContainsLower(row.Repo+"#"+n, q) {
					continue
				}
			} else if !strings.Contains(n, q) && !dictionaryLineContainsLower(row.Repo, q) {
				continue
			}
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

func TestIssueSearchQualifiedKeyAllocationBudget(t *testing.T) {
	for _, repo := range []string{
		"Example/" + strings.Repeat("Repository", 8),
		"例子/" + strings.Repeat("仓库", 32),
		"Example/Repo#" + strings.Repeat("Fragment", 16),
	} {
		s := issueKeySearchServer(1000, repo)
		for _, query := range []string{"missing#1000", "#100", "y#1000", "库#1000", "#fragment", "fragment#1000"} {
			want := previousSearchIssues(s, query)
			var got []account.AdminSearchHit
			allocations := testing.AllocsPerRun(100, func() { got = s.searchIssues(query) })
			if !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("仓库 %q、查询 %q 的结果变化：got=%#v want=%#v", repo, query, got, want)
			}
			budget := 0.0
			if len(want) > 0 {
				// 仅结果标识、标题及返回切片需要分配。
				budget = float64(2*len(want) + 1)
			}
			if allocations > budget {
				t.Fatalf("仓库 %q、查询 %q 分配 %.0f 次，期望最多 %.0f 次", repo, query, allocations, budget)
			}
		}
	}
}

func TestIssueSearchQualifiedKeyUnicodeMatchesPrevious(t *testing.T) {
	for _, repo := range []string{
		"Example/K", "Example/İ", "Example/Σ", "Example/ς", "Example/ẞ",
		"Example/仓库", "Example/𐐀", "Example/\xff", "Example/\xe2\x82", "Example/�",
		"Example/A##B#", "", "#",
	} {
		s := issueKeySearchServer(20, repo)
		queries := []string{"#", "##", "#100", "k#1000", "i#1000", "σ#1000", "ς#1000", "ss#1000", "ß#1000", "�#1000", "\xff#1000", "#b#1000", "#b", "##b#"}
		lowerKey := strings.ToLower(repo + "#1000")
		// 穷举归一化仓库键的字符边界子串，覆盖查询完全位于仓库内和跨分隔符的情况。
		for start := range lowerKey {
			for end := range lowerKey {
				if end > start {
					queries = append(queries, lowerKey[start:end])
				}
			}
			queries = append(queries, lowerKey[start:])
		}
		for _, query := range queries {
			want := previousSearchIssues(s, query)
			got := s.searchIssues(query)
			if !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("仓库 %q、查询 %q 的结果变化：got=%#v want=%#v", repo, query, got, want)
			}
		}
	}
}
