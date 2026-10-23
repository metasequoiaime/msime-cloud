package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func dictCaseSearchServer(count int, title, note string) *Server {
	s := &Server{}
	st := s.dictPRState()
	for i := range count {
		st.index = append(st.index, dictPRSummary{Number: i + 1, Title: title, Note: note})
	}
	return s
}

func TestDictPRSearchCaseNormalizationAllocationBudget(t *testing.T) {
	for _, tc := range []struct{ name, title, note string }{
		{"title", "Fix Candidate Window", "submitter note"},
		{"note", "candidate window", "Submitted By Example"},
		{"both", "Fix Candidate Window", "Submitted By Example"},
		{"unicode", "候选词 KİΣẞ 𐐀", "备注 KİΣẞ 𐐀"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const count = 20
			s := dictCaseSearchServer(count, tc.title, tc.note)
			allocations := testing.AllocsPerRun(100, func() {
				if got := s.searchDictPRs("does-not-match"); got != nil {
					t.Fatalf("未命中词库投稿搜索返回了结果：%#v", got)
				}
			})
			// 未命中记录不应构造显示标题或小写副本。
			if allocations != 0 {
				t.Fatalf("未命中词库投稿搜索分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func TestDictPRSearchCaseNormalizationMatchesPrevious(t *testing.T) {
	for _, text := range []string{
		"Fix Candidate Window", "候选词 KİΣẞ 𐐀", "lowercase title", "",
		strings.Repeat("候选词", 200) + "WINDOW KİΣ", "\xffWINDOW\xfe",
	} {
		for _, note := range []string{"", text} {
			s := dictCaseSearchServer(20, text, note)
			for _, query := range []string{
				"WINDOW", "candidate", "候选词", "i", "K", "Σ", "σ", "ς", "ß", "ẞ", "𐐨", "�",
				"#1", "#1 fix", "1 fix", "1 候选词", "#20", "does-not-match", "  ", strings.Repeat("x", 1100),
			} {
				want := previousUnboundedSearchDictPRs(s, query)
				want = want[:min(len(want), adminSearchLimit)]
				got := s.searchDictPRs(query)
				if !slices.Equal(got, want) || (got == nil) != (want == nil) {
					t.Fatalf("标题 %q、备注 %q、查询 %q 的结果或顺序变化：got=%#v want=%#v", text, note, query, got, want)
				}
			}
		}
	}
}

func BenchmarkDictPRSearchCaseNormalization(b *testing.B) {
	for _, corpus := range []struct{ name, title, note string }{
		{"ascii", "Fix Candidate Window", "Submitted By Example"},
		{"unicode", "候选词 KİΣẞ 𐐀", "备注 KİΣẞ 𐐀"},
	} {
		for _, count := range []int{20, 1000} {
			s := dictCaseSearchServer(count, corpus.title, corpus.note)
			for _, query := range []string{"does-not-match", "submitted", "候选词"} {
				want := previousDictPRSearchCaseNormalization(s, query)
				for _, tc := range []struct {
					name string
					fn   func(*Server, string) []account.AdminSearchHit
				}{
					{"previous", previousDictPRSearchCaseNormalization},
					{"current", (*Server).searchDictPRs},
				} {
					b.Run(corpus.name+"/"+strconv.Itoa(count)+"/"+query+"/"+tc.name, func(b *testing.B) {
						if got := tc.fn(s, query); !slices.Equal(got, want) || (got == nil) != (want == nil) {
							b.Fatal("词库投稿搜索结果变化")
						}
						b.ReportAllocs()
						for b.Loop() {
							if got := tc.fn(s, query); len(got) != len(want) {
								b.Fatal("词库投稿搜索结果数量变化")
							}
						}
					})
				}
			}
		}
	}
}

func previousDictPRSearchCaseNormalization(s *Server, q string) []account.AdminSearchHit {
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
		// 有备注时，用投稿备注作为搜索结果标题。
		if p.Note != "" {
			title = "#" + strconv.Itoa(p.Number) + " 词库：" + p.Note
		}
		hits = append(hits, account.AdminSearchHit{Kind: "dict_pr", ID: strconv.Itoa(p.Number), Title: title, Where: "词库审核", Target: "dictpr"})
		// 全局搜索最多使用前 `adminSearchLimit` 条命中，及时停止构造其余结果。
		if len(hits) == adminSearchLimit {
			break
		}
	}
	return hits
}
