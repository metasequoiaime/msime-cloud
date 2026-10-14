package account

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminUserResourceCountsUseOneAggregate(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "admin-resource-count-owner@example.test"})
	other := complete(t, db, Identity{"email", "admin-resource-count-other@example.test"})
	empty := complete(t, db, Identity{"email", "admin-resource-count-empty@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation)
 SELECT 'resource-count-'||n,$1,CASE WHEN n<=2 THEN 'dictionary' WHEN n<=5 THEN 'reply' ELSE 'phrase' END,'合成作品','{}'::jsonb,
 CASE n%3 WHEN 0 THEN 'removed' WHEN 1 THEN 'pending' ELSE 'approved' END FROM generate_series(1,9) n
 UNION ALL SELECT 'resource-count-other-'||n,$2,'dictionary','其他账号','{}'::jsonb,'approved' FROM generate_series(1,3) n`, owner.User.ID, other.User.ID); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "auth_users u", queries: make(chan communityPageQuery, 2)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, user := range []string{owner.User.ID, empty.User.ID} {
		t.Run(user, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.adminUser(w, httptest.NewRequest("GET", "/api/users/"+user, nil), user)
			var detail struct {
				ID           string `json:"id"`
				Dictionaries int    `json:"dictionaries"`
				Replies      int    `json:"replies"`
				Phrases      int    `json:"phrases"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.ID != user {
				t.Fatal(w.Code, w.Body.String())
			}
			counts := [3]int{detail.Dictionaries, detail.Replies, detail.Phrases}
			want := [3]int{2, 3, 4}
			if user == empty.User.ID {
				want = [3]int{}
			}
			if counts != want {
				t.Fatalf("资源计数为 %v，期望 %v", counts, want)
			}
			var query communityPageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到用户详情查询")
			}
			var raw json.RawMessage
			if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type node struct {
				Relation string `json:"Relation Name"`
				Plans    []node `json:"Plans"`
			}
			var plans []struct{ Plan node }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal(err, string(raw))
			}
			scans := 0
			var visit func(node)
			visit = func(n node) {
				if n.Relation == "community_resources" {
					scans++
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			// 三类计数共用一次聚合，最近作品元数据另外扫描一次。
			if scans != 2 {
				t.Errorf("资源表扫描了 %d 次，期望 2 次", scans)
			}
		})
	}
}
