package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestDictPRSearchUnmatchedTitleAllocationBudget(t *testing.T) {
	for _, text := range []string{
		"Fix Candidate Window", "候选词 KİΣẞ 𐐀", "\xffWINDOW\xfe",
		strings.Repeat("候选词", 200) + "WINDOW KİΣ",
	} {
		for _, count := range []int{20, 1000} {
			s := dictCaseSearchServer(count, text, text)
			for _, query := range []string{"does-not-match", "9999999 impossible", "#9999999 候选词"} {
				allocations := testing.AllocsPerRun(20, func() {
					if got := s.searchDictPRs(query); got != nil {
						t.Fatalf("未命中词库投稿搜索返回了结果：%#v", got)
					}
				})
				if allocations != 0 {
					t.Fatalf("标题 %q、%d 条记录、查询 %q 分配 %.0f 次，期望零分配", text, count, query, allocations)
				}
			}
		}
	}
}

func TestDictPRSearchMatchedTitleAllocationBudget(t *testing.T) {
	for _, note := range []string{"", "投稿备注"} {
		s := dictCaseSearchServer(20, "Candidate Window", note)
		for i := range s.dictPRState().index {
			s.dictPRState().index[i].Number += 1000
		}
		want := previousUnboundedSearchDictPRs(s, "window")[:adminSearchLimit]
		var got []account.AdminSearchHit
		allocations := testing.AllocsPerRun(100, func() { got = s.searchDictPRs("window") })
		if !slices.Equal(got, want) {
			t.Fatalf("备注 %q 的搜索结果变化：got=%#v want=%#v", note, got, want)
		}
		// 每条结果各分配一次编号和标题，返回切片允许最多四次增长。
		budget := float64(2*len(want) + 4)
		if allocations > budget {
			t.Fatalf("备注 %q 的搜索分配 %.0f 次，期望最多 %.0f 次", note, allocations, budget)
		}
	}
}

func TestDictPRSearchTitlePartsMatchPrevious(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, title := range []string{
		"Fix Candidate Window", "候选词 KİΣẞ 𐐀", " Σς仓库", "TWO  Words", "", "#1000 空格", "\xffWINDOW\xe2\x82",
	} {
		for _, note := range []string{"", "投稿备注 WINDOW"} {
			s := &Server{}
			for _, number := range []int{-1000, 0, 99, 100, 1000, 1001, maxInt, -maxInt - 1, 123} {
				s.dictPRState().index = append(s.dictPRState().index, dictPRSummary{Number: number, Title: title, Note: note})
			}
			queries := []string{
				"WINDOW", "#100", "100", "0 空格", "#1000 ", "9999999 impossible", "WINDOW NOTE", "词库", "�", "  ",
				"0 Fix", "0 候选", "0  σ", "0  ς", "1000 two  words", "投稿备注", strconv.Itoa(maxInt), strconv.Itoa(-maxInt - 1),
			}
			key := strings.ToLower("#1000 " + title)
			// 穷举字符边界子串，验证编号内、标题内和跨空格的匹配语义。
			for start := range key {
				for end := range key {
					if end > start {
						queries = append(queries, key[start:end])
					}
				}
				queries = append(queries, key[start:])
			}
			for _, query := range queries {
				want := previousUnboundedSearchDictPRs(s, query)
				want = want[:min(len(want), adminSearchLimit)]
				got := s.searchDictPRs(query)
				if !slices.Equal(got, want) || (got == nil) != (want == nil) {
					t.Fatalf("标题 %q、备注 %q、查询 %q 的结果或顺序变化：got=%#v want=%#v", title, note, query, got, want)
				}
			}
		}
	}
}

func BenchmarkDictPRSearchTitleConstruction(b *testing.B) {
	for _, corpus := range []struct{ name, title, note string }{
		{"ascii", "Fix Candidate Window", "Submitted By Example"},
		{"unicode", "候选词 KİΣẞ 𐐀", "备注 KİΣẞ 𐐀"},
	} {
		s := dictCaseSearchServer(1000, corpus.title, corpus.note)
		for _, query := range []string{"does-not-match", "9999999 impossible", "#1000", "submitted", "候选词", "1 fix"} {
			want := previousDictPRSearchTitleConstruction(s, query)
			for _, tc := range []struct {
				name string
				fn   func(*Server, string) []account.AdminSearchHit
			}{
				{"previous", previousDictPRSearchTitleConstruction},
				{"current", (*Server).searchDictPRs},
			} {
				b.Run(corpus.name+"/"+query+"/"+tc.name, func(b *testing.B) {
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

func previousDictPRSearchTitleConstruction(s *Server, q string) []account.AdminSearchHit {
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
		// 复用词条查询的栈上小写匹配，避免逐条复制标题和投稿备注。
		if !dictionaryLineContainsLower(title, q) && !dictionaryLineContainsLower(p.Note, q) {
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
