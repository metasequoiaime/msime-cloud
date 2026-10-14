package account

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminResourceRatingStatisticsUseOneScan(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "admin-rating-owner@example.test"})
	reader := complete(t, db, Identity{"email", "admin-rating-reader@example.test"})
	other := complete(t, db, Identity{"email", "admin-rating-other@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content)
 VALUES('rating-rated',$1,'dictionary','example rated','{"entries":[]}'),('rating-empty',$1,'dictionary','example empty','{"entries":[]}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resource_ratings(resource_id,user_id,stars) VALUES('rating-rated',$1,3),('rating-rated',$2,5),('rating-rated',$3,2)`, owner.User.ID, reader.User.ID, other.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_resources s JOIN auth_users u", queries: make(chan communityPageQuery, 8)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, id := range []string{"rating-rated", "rating-empty"} {
		t.Run(id, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/dictionaries/"+id, nil)
			w := httptest.NewRecorder()
			a.adminContent(w, r, "dictionaries", id)
			var item struct {
				ID      string  `json:"id"`
				Count   int     `json:"rating_count"`
				Average float64 `json:"rating_average"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			count, average := 3, 10.0/3
			if id == "rating-empty" {
				count, average = 0, 0
			}
			if item.ID != id || item.Count != count || math.Abs(item.Average-average) > 1e-12 {
				t.Fatal("评分统计错误：", item)
			}
			query := <-trace.queries
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
				if n.Relation == "community_resource_ratings" {
					scans++
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			if scans != 1 {
				t.Errorf("评分统计扫描了 %d 次评分表，期望 1 次", scans)
			}
		})
	}
}
