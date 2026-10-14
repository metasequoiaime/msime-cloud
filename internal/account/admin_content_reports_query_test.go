package account

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminContentReportsUseNewestIndex(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "report-index-owner@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('report-index-skin',$1,'举报索引测试','{}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id,display_name) SELECT 'report-index-user-'||n,'举报者' FROM generate_series(1,1000) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_reports(kind,item_id,reporter_id,reason,created_at) SELECT 'skins','report-index-skin','report-index-user-'||n,'spam',now()+n*interval '1 second' FROM generate_series(1,1000) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `ANALYZE community_reports; ANALYZE auth_users`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "community_reports r", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	w := httptest.NewRecorder()
	a.adminContent(w, httptest.NewRequest("GET", "/api/skins/report-index-skin", nil), "skins", "report-index-skin")
	var detail struct {
		Reports []contentReport `json:"reports"`
		Count   int             `json:"report_count"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || len(detail.Reports) != 100 || detail.Count != 1000 {
		t.Fatal(w.Code, w.Body.String())
	}
	for i := 1; i < len(detail.Reports); i++ {
		if !detail.Reports[i-1].CreatedAt.After(detail.Reports[i].CreatedAt) {
			t.Fatal("举报列表没有按时间降序排列")
		}
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到举报查询")
	}
	var planRaw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&planRaw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		NodeType  string `json:"Node Type"`
		IndexName string `json:"Index Name"`
		Plans     []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(planRaw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(planRaw))
	}
	var sort, newest bool
	var visit func(node)
	visit = func(n node) {
		if n.NodeType == "Sort" {
			sort = true
		}
		if n.IndexName == "community_reports_item_newest" {
			newest = true
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if !newest || sort {
		t.Fatalf("举报查询未使用按时间索引：newest=%t sort=%t plan=%s", newest, sort, planRaw)
	}
}
