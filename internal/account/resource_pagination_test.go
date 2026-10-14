package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resourcePageQuery struct {
	sql  string
	args []any
}

type resourcePageTrace struct{ queries chan resourcePageQuery }

func (trace *resourcePageTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM community_resources") {
		trace.queries <- resourcePageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*resourcePageTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestResourcePaginationLimitsRatingQueries(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "page-owner@example.test"})
	reader := complete(t, db, Identity{"email", "page-reader@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation,created_at)
 SELECT 'page-'||lpad(n::text,3,'0'),$1,'dictionary','page example','{"entries":[]}',
 CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,'2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,60) n`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('page-reply',$1,'reply','page example','{"prompt":"example"}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resource_saves(resource_id,user_id) SELECT id,$1 FROM community_resources WHERE kind='dictionary'`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, rating := range []struct {
		user  string
		stars int
	}{{owner.User.ID, 3}, {reader.User.ID, 5}} {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resource_ratings(resource_id,user_id,stars) SELECT id,$1,$2 FROM community_resources WHERE kind='dictionary'`, rating.user, rating.stars); err != nil {
			t.Fatal(err)
		}
	}
	cfg := db.pool.Config()
	trace := &resourcePageTrace{queries: make(chan resourcePageQuery, 8)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		scope, token string
		owned        bool
		saved        bool
		myRating     int
	}{
		{"", "", false, false, 0},
		{"mine", owner.AccessToken, true, false, 3},
		{"saved", reader.AccessToken, false, true, 5},
	} {
		t.Run("scope="+tc.scope, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/community/resources?kind=dictionary&q=page&offset=20&fields=moderation&scope="+tc.scope, nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			a.resourceList(w, r)
			var body struct {
				Items   []CommunityResource `json:"items"`
				HasMore bool                `json:"has_more"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 20 || !body.HasMore {
				t.Fatal(w.Code, w.Body.String())
			}
			var ids []string
			for n := 60; n > 0; n-- {
				if tc.owned || n%10 != 0 {
					ids = append(ids, fmt.Sprintf("page-%03d", n))
				}
			}
			for i, item := range body.Items {
				if item.ID != ids[20+i] || item.Saves != 1 || item.Saved != tc.saved || item.Owned != tc.owned || item.RatingCount != 2 || item.RatingAverage != 4 || item.MyRating != tc.myRating {
					t.Fatal("分页顺序或统计字段错误：", i, item)
				}
				moderation := ""
				if tc.owned {
					moderation = "approved"
					if strings.HasSuffix(item.ID, "0") {
						moderation = "removed"
					}
				}
				if item.Moderation != moderation {
					t.Fatal("分页审核状态可见性错误：", item)
				}
			}
			var query resourcePageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到资源目录查询")
			}
			tx, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			// 固定连接顺序，验证评分不会因优化器重排而先于分页关联执行。
			if _, err := tx.Exec(t.Context(), `SET LOCAL join_collapse_limit=1`); err != nil {
				t.Fatal(err)
			}
			var raw json.RawMessage
			if err := tx.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
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
			// 评分只应为本页的 21 条候选执行；OFFSET 跳过的条目不需要读取评分。
			ratingScans := 0
			var visit func(node)
			visit = func(n node) {
				if n.Relation == "community_resource_ratings" {
					ratingScans++
					if n.Loops > 21 {
						t.Errorf("评分查询为被跳过的资源执行：%d 次", n.Loops)
					}
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			if ratingScans == 0 {
				t.Fatal("执行计划缺少评分查询")
			}
		})
	}
	w := httptest.NewRecorder()
	a.resourceList(w, httptest.NewRequest("GET", "/v1/community/resources?kind=dictionary&offset=1000", nil))
	var empty struct {
		Items   []CommunityResource `json:"items"`
		HasMore bool                `json:"has_more"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &empty) != nil || empty.Items == nil || len(empty.Items) != 0 || empty.HasMore {
		t.Fatal("目录末尾应返回空数组：", w.Code, w.Body.String())
	}
}
