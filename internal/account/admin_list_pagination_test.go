package account

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminUnfilteredListLimitsItemQueries(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "admin-page-owner@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design,moderation,moderation_reason,category,created_at)
 SELECT 'page-'||lpad(n::text,3,'0'),$1,'page example','{"background":12,"photo":"cGhvdG8="}',
 CASE WHEN n%10=0 THEN 'removed' WHEN n%3=0 THEN 'pending' ELSE 'approved' END,'example reason',
 CASE WHEN n%2=0 THEN 'nature' ELSE 'other' END,'2026-01-01'::timestamptz+((n+1)/2)*interval '1 second'
 FROM generate_series(1,125) n`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skin_downloads(skin_id,user_id) SELECT id,$1 FROM community_skins`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_reports(kind,item_id,reporter_id,reason) SELECT 'skins',id,$1,'example' FROM community_skins`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_skins", queries: make(chan communityPageQuery, 8)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a, baseline := &Service{store: &Store{pool: pool}}, &Service{store: db}
	for _, page := range []int{1, 2, 3, 4} {
		for _, params := range []string{"", "&q=&category=&status="} {
			t.Run(fmt.Sprintf("page=%d%s", page, params), func(t *testing.T) {
				w := httptest.NewRecorder()
				a.AdminHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/skins?page=%d%s", page, params), nil))
				old := httptest.NewRecorder()
				// 所有名称都匹配 page；带搜索的查询提供原有完整条目形状和分页结果。
				baseline.AdminHTTP(old, httptest.NewRequest("GET", fmt.Sprintf("/api/skins?page=%d&q=page", page), nil))
				var got, want any
				if w.Code != 200 || old.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || json.Unmarshal(old.Body.Bytes(), &want) != nil || !reflect.DeepEqual(got, want) {
					t.Fatal("空筛选与全匹配搜索结果不同：", w.Code, w.Body.String(), old.Code, old.Body.String())
				}
				var body struct {
					Items []struct {
						ID                 string `json:"id"`
						Downloads, Reports int
					} `json:"items"`
					Total int  `json:"total"`
					Page  int  `json:"page"`
					More  bool `json:"has_more"`
				}
				if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Items == nil || body.Total != 125 || body.Page != page || len(body.Items) != min(50, max(0, 125-(page-1)*50)) || body.More != (page*50 < 125) {
					t.Fatal("分页计数错误：", w.Body.String())
				}
				for i, item := range body.Items {
					if item.ID != fmt.Sprintf("page-%03d", 125-(page-1)*50-i) || item.Downloads != 1 || item.Reports != 1 {
						t.Fatal("分页顺序或互动统计错误：", item)
					}
				}
				query := <-trace.queries
				var raw json.RawMessage
				if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				type node struct {
					Relation string `json:"Relation Name"`
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
					if n.Relation == "community_skin_downloads" || n.Relation == "community_reports" {
						seen[n.Relation] = true
						if n.Loops > 50 {
							t.Errorf("%s 为页外条目执行：%d 次", n.Relation, n.Loops)
						}
					}
					for _, child := range n.Plans {
						visit(child)
					}
				}
				visit(plans[0].Plan)
				if !seen["community_skin_downloads"] || !seen["community_reports"] {
					t.Fatal("缺少互动查询计划")
				}
			})
		}
	}
}
