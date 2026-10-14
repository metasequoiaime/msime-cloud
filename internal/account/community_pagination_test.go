package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communityPageQuery struct {
	sql  string
	args []any
}

type communityPaginationTrace struct {
	table   string
	queries chan communityPageQuery
}

func (trace *communityPaginationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM "+trace.table+" ") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*communityPaginationTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func assertCommunityPageWork(t *testing.T, db *Store, trace *communityPaginationTrace, relations ...string) {
	t.Helper()
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到目录查询")
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Subplan  string `json:"Subplan Name"`
		Loops    int    `json:"Actual Loops"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	seen := map[string]bool{}
	var visit func(node)
	visit = func(n node) {
		// 页内筛选可能需要读取更多收藏关系；这里只限制返回条目的逐条统计。
		if strings.HasPrefix(n.Subplan, "CTE ") {
			return
		}
		if slices.Contains(relations, n.Relation) {
			seen[n.Relation] = true
			// 逐条统计只应为本页的 21 条候选执行，不应读取 OFFSET 跳过条目的互动表。
			if n.Loops > 21 {
				t.Errorf("%s 为被跳过的条目执行：%d 次", n.Relation, n.Loops)
			}
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	for _, relation := range relations {
		if !seen[relation] {
			t.Errorf("执行计划缺少 %s 查询", relation)
		}
	}
}

func TestKeyboardPaginationLimitsInteractionQueries(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "keyboard-page-owner@example.test"})
	reader := complete(t, db, Identity{"email", "keyboard-page-reader@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design,moderation,category,created_at)
 SELECT 'page-'||lpad(n::text,3,'0'),$1,CASE WHEN n>65 THEN 'different' ELSE 'page example' END,'{"background":12,"photo":"cGhvdG8="}',
 CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,CASE WHEN n>60 THEN 'nature' ELSE 'other' END,
 '2026-01-01'::timestamptz+((n+1)/2)*interval '1 second' FROM generate_series(1,70) n`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{owner.User.ID, reader.User.ID} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skin_saves(skin_id,user_id,created_at)
 SELECT id,$1,'2026-02-01'::timestamptz-((right(id,3)::int+1)/2)*interval '1 second' FROM community_skins`, user); err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skin_downloads(skin_id,user_id) SELECT id,$1 FROM community_skins`, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, rating := range []struct {
		user  string
		stars int
	}{{owner.User.ID, 3}, {reader.User.ID, 5}} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skin_ratings(skin_id,user_id,stars) SELECT id,$1,$2 FROM community_skins`, rating.user, rating.stars); err != nil {
			t.Fatal(err)
		}
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_skins", queries: make(chan communityPageQuery, 16)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		name, scope, token string
		owned, saved       bool
		myRating           int
	}{
		{"public", "", "", false, false, 0},
		{"mine", "mine", owner.AccessToken, true, true, 3},
		{"saved-reader", "saved", reader.AccessToken, false, true, 5},
		{"saved-owner", "saved", owner.AccessToken, true, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ids []string
			if tc.scope == "saved" {
				for n := 1; n <= 60; n++ {
					if tc.owned || n%10 != 0 {
						ids = append(ids, fmt.Sprintf("page-%03d", n))
					}
				}
			} else {
				for n := 59; n > 0; n -= 2 {
					for _, value := range []int{n, n + 1} {
						if tc.owned || value%10 != 0 {
							ids = append(ids, fmt.Sprintf("page-%03d", value))
						}
					}
				}
			}
			for _, offset := range []int{20, 40, 1000} {
				r := httptest.NewRequest("GET", fmt.Sprintf("/v1/community/skins?q=page&category=other&include=category&fields=saved,moderation&scope=%s&offset=%d", tc.scope, offset), nil)
				r.Header.Set("Authorization", "Bearer "+tc.token)
				w := httptest.NewRecorder()
				a.communityList(w, r)
				var body struct {
					Skins   []CommunitySkin `json:"skins"`
					HasMore bool            `json:"has_more"`
				}
				want := ids[min(offset, len(ids)):min(offset+20, len(ids))]
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Skins == nil || len(body.Skins) != len(want) || body.HasMore != (offset+20 < len(ids)) {
					t.Fatal("分页长度错误：", offset, w.Code, w.Body.String())
				}
				for i, item := range body.Skins {
					if item.ID != want[i] || item.Downloads != 2 || item.RatingCount != 2 || item.RatingAverage != 4 || item.MyRating != tc.myRating || item.Owned != tc.owned || item.Category != "other" || item.Author == "" || item.Saved == nil || *item.Saved != tc.saved || item.Saves == nil || *item.Saves != 2 || strings.Contains(string(item.Design), "photo") {
						t.Fatal("分页顺序或条目字段错误：", offset, i, item)
					}
					moderation := ""
					if tc.owned {
						moderation = "approved"
						if strings.HasSuffix(item.ID, "0") {
							moderation = "removed"
						}
					}
					if item.Moderation != moderation {
						t.Fatal("审核状态可见性错误：", item)
					}
				}
				assertCommunityPageWork(t, db, trace, "community_skin_downloads", "community_skin_ratings", "community_skin_saves")
			}
		})
	}
}
