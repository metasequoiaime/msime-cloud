package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func dictSearchLimitServer(count int) *Server {
	s := &Server{}
	st := s.dictPRState()
	for i := range count {
		p := dictPRSummary{Number: i + 1, Title: "feat(custom): add 1 word"}
		if i%2 == 0 {
			p.Note = "候选词条"
		}
		st.index = append(st.index, p)
	}
	if count > adminSearchLimit {
		st.index[count-1].Note = "最后一条投稿"
	}
	return s
}

func TestDictPRSearchUsesGlobalResultLimit(t *testing.T) {
	s := dictSearchLimitServer(100)
	for _, query := range []string{"FEAT(CUSTOM)", "候选词条", "#100", "最后一条", "不存在的内容", "", "  "} {
		t.Run(query, func(t *testing.T) {
			all := previousUnboundedSearchDictPRs(s, query)
			want := all[:min(len(all), adminSearchLimit)]
			got := s.searchDictPRs(query)
			if len(got) != len(want) {
				t.Fatalf("词库投稿搜索返回 %d 条，期望 %d 条", len(got), len(want))
			}
			if !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("词库投稿搜索结果应保留最前面的 %d 条命中：got=%#v want=%#v", adminSearchLimit, got, want)
			}
			for _, peers := range [][]account.AdminSearchHit{nil, {{Kind: "user", ID: "peer-1"}, {Kind: "user", ID: "peer-2"}}} {
				before := interleaveSearchHits(adminSearchLimit, peers, all)
				after := interleaveSearchHits(adminSearchLimit, peers, got)
				if !slices.Equal(before, after) {
					t.Fatalf("全局搜索交错结果变化：got=%#v want=%#v", after, before)
				}
			}
		})
	}
}

func TestDictPRSearchStopsAllocatingAfterResultLimit(t *testing.T) {
	allocationCount := func(count int) float64 {
		s := dictSearchLimitServer(count)
		return testing.AllocsPerRun(100, func() { _ = s.searchDictPRs("feat(custom)") })
	}
	budget := allocationCount(adminSearchLimit)
	for _, count := range []int{20, 100, 1000} {
		if got := allocationCount(count); got > budget {
			t.Fatalf("前 %d 条已命中时，索引从 %d 条增长到 %d 条使分配从 %.0f 次增至 %.0f 次", adminSearchLimit, adminSearchLimit, count, budget, got)
		}
	}
}

func BenchmarkDictPRSearchGlobalResultLimit(b *testing.B) {
	for _, count := range []int{0, 1, 8, 20, 100} {
		s := dictSearchLimitServer(count)
		for _, tc := range []struct {
			name string
			fn   func(*Server, string) []account.AdminSearchHit
		}{
			{"previous", previousUnboundedSearchDictPRs},
			{"current", (*Server).searchDictPRs},
		} {
			b.Run(strconv.Itoa(count)+"/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got := interleaveSearchHits(adminSearchLimit, tc.fn(s, "feat(custom)"))
					if len(got) != min(count, adminSearchLimit) {
						b.Fatal("全局搜索结果数量变化")
					}
				}
			})
		}
	}
}

func previousUnboundedSearchDictPRs(s *Server, q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	st := s.dictPRState()
	st.mu.Lock()
	defer st.mu.Unlock()
	var hits []account.AdminSearchHit
	for _, p := range st.index {
		title := "#" + strconv.Itoa(p.Number) + " " + p.Title
		if !strings.Contains(strings.ToLower(title), q) && !strings.Contains(strings.ToLower(p.Note), q) {
			continue
		}
		if p.Note != "" {
			title = "#" + strconv.Itoa(p.Number) + " 词库：" + p.Note
		}
		hits = append(hits, account.AdminSearchHit{Kind: "dict_pr", ID: strconv.Itoa(p.Number), Title: title, Where: "词库审核", Target: "dictpr"})
	}
	return hits
}
