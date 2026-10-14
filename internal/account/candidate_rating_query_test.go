package account

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCandidateRatingStatisticsUseOneScan(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "candidate-rating-owner@example.test"})
	reader := complete(t, db, Identity{"email", "candidate-rating-reader@example.test"})
	other := complete(t, db, Identity{"email", "candidate-rating-other@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256)
 VALUES('00000000-0000-4000-8000-000000000001',$1,'rating-rated','example rated','1','example assets','x'::bytea,'preview.png',repeat('a',64)),
 ('00000000-0000-4000-8000-000000000002',$1,'rating-empty','example empty','1','example assets','x'::bytea,'preview.png',repeat('b',64))`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_ratings(skin_id,user_id,stars)
 VALUES('00000000-0000-4000-8000-000000000001',$1,3),('00000000-0000-4000-8000-000000000001',$2,5),('00000000-0000-4000-8000-000000000001',$3,2)`, owner.User.ID, reader.User.ID, other.User.ID); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &communityPaginationTrace{table: "community_candidate_skins s", queries: make(chan communityPageQuery, 8)}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, id := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"} {
		for _, viewer := range []struct {
			name, token string
			mine        int
			owned       bool
		}{{"anonymous", "", 0, false}, {"reader", reader.AccessToken, 5, false}, {"owner", owner.AccessToken, 3, true}} {
			t.Run(id+"/"+viewer.name, func(t *testing.T) {
				r := httptest.NewRequest("GET", "/v1/community/candidate-skins/"+id, nil)
				r.SetPathValue("id", id)
				r.Header.Set("Authorization", "Bearer "+viewer.token)
				w := httptest.NewRecorder()
				a.communityCandidateDetail(w, r)
				var item CommunityCandidateSkin
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
					t.Fatal(w.Code, w.Body.String())
				}
				count, average, mine := 3, 10.0/3, viewer.mine
				if id == "00000000-0000-4000-8000-000000000002" {
					count, average, mine = 0, 0, 0
				}
				if item.ID != id || item.RatingCount != count || math.Abs(item.RatingAverage-average) > 1e-12 || item.MyRating != mine || item.Owned != viewer.owned {
					t.Fatal("评分统计或个人字段错误：", item)
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
					if n.Relation == "community_candidate_skin_ratings" {
						scans++
					}
					for _, child := range n.Plans {
						visit(child)
					}
				}
				visit(plans[0].Plan)
				// 人数、平均分和个人评分应在同一次评分表扫描中得到。
				if scans != 1 {
					t.Errorf("评分统计扫描了 %d 次评分表，期望 1 次", scans)
				}
			})
		}
	}
}
