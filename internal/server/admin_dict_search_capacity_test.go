package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func dictCapacitySearchServer(count int, note string) *Server {
	s := dictCaseSearchServer(count, "Candidate Window", note)
	for i := range s.dictPRState().index {
		s.dictPRState().index[i].Number += 1000
	}
	return s
}

func TestDictPRSearchResultAllocationBudget(t *testing.T) {
	for _, corpus := range []struct{ name, note string }{
		{"title", ""}, {"note", "投稿备注"},
	} {
		for _, count := range []int{0, 1, 5, 8, 20} {
			t.Run(corpus.name+"/"+strconv.Itoa(count), func(t *testing.T) {
				s := dictCapacitySearchServer(count, corpus.note)
				want := previousDictPRSearchResultCapacity(s, "window")
				var got []account.AdminSearchHit
				allocations := testing.AllocsPerRun(100, func() { got = s.searchDictPRs("window") })
				if !slices.Equal(got, want) || (got == nil) != (want == nil) {
					t.Fatalf("词库投稿搜索结果或顺序变化：got=%#v want=%#v", got, want)
				}
				budget := 0.0
				if len(want) > 0 {
					// 每条结果各分配一次编号和标题，返回切片最多分配一次。
					budget = float64(2*len(want) + 1)
				}
				if allocations > budget {
					t.Fatalf("词库投稿搜索分配 %.0f 次，期望最多 %.0f 次", allocations, budget)
				}
				if len(got) > 0 {
					got[0].Title = "调用方修改结果"
					if again := s.searchDictPRs("window"); !slices.Equal(again, want) {
						t.Fatalf("返回结果修改影响后续查询：%#v", again)
					}
				}
			})
		}
	}
}

func BenchmarkDictPRSearchResultCapacity(b *testing.B) {
	for _, corpus := range []struct{ name, note string }{
		{"title", ""}, {"note", "投稿备注"},
	} {
		for _, count := range []int{0, 1, 5, 8, 1000} {
			s := dictCapacitySearchServer(count, corpus.note)
			want := previousDictPRSearchResultCapacity(s, "window")
			for _, tc := range []struct {
				name string
				fn   func(*Server, string) []account.AdminSearchHit
			}{
				{"previous", previousDictPRSearchResultCapacity},
				{"current", (*Server).searchDictPRs},
			} {
				b.Run(corpus.name+"/"+strconv.Itoa(count)+"/"+tc.name, func(b *testing.B) {
					if got := tc.fn(s, "window"); !slices.Equal(got, want) || (got == nil) != (want == nil) {
						b.Fatal("词库投稿搜索结果变化")
					}
					b.ReportAllocs()
					for b.Loop() {
						if got := tc.fn(s, "window"); len(got) != len(want) {
							b.Fatal("词库投稿搜索结果数量变化")
						}
					}
				})
			}
		}
	}
}

func previousDictPRSearchResultCapacity(s *Server, q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	queryNumber, queryTitle, queryHasSpace := strings.Cut(q, " ")
	st := s.dictPRState()
	st.mu.Lock()
	defer st.mu.Unlock()
	// `#` 加有符号 64 位整数最多占 21 字节，仅用于匹配，不保存在结果中。
	var numberBuffer [21]byte
	numberBuffer[0] = '#'
	var hits []account.AdminSearchHit
	for _, p := range st.index {
		if !dictionaryLineContainsLower(p.Title, q) && !dictionaryLineContainsLower(p.Note, q) {
			prefix := string(strconv.AppendInt(numberBuffer[:1], int64(p.Number), 10))
			matched := strings.Contains(prefix, q)
			if queryHasSpace {
				// 编号不含空格，跨分隔符的查询只能以第一个空格对齐。
				matched = strings.HasSuffix(prefix, queryNumber) && qqCommandPrefixEqualFold(p.Title, queryTitle)
			}
			if !matched {
				continue
			}
		}
		// 仅命中后构造显示标题；有备注时以投稿备注命名。
		number := strconv.Itoa(p.Number)
		var title string
		if p.Note != "" {
			title = "#" + number + " 词库：" + p.Note
		} else {
			title = "#" + number + " " + p.Title
		}
		hits = append(hits, account.AdminSearchHit{Kind: "dict_pr", ID: number, Title: title, Where: "词库审核", Target: "dictpr"})
		// 全局搜索最多使用前 `adminSearchLimit` 条命中，及时停止构造其余结果。
		if len(hits) == adminSearchLimit {
			break
		}
	}
	return hits
}
