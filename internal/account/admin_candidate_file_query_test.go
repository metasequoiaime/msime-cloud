package account

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminCandidateSkinFileStatsRunOnlyForPage(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id,display_name) VALUES('candidate-owner','合成作者')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),'candidate-owner','skin-'||n,'候选皮肤','1','CC0',decode('01','hex'),'preview.png',repeat('a',64),now()+n*interval '1 second' FROM generate_series(1,121) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_candidate_skin_files(skin_id,path,bytes)
 SELECT s.id,'image-'||n||'.png',decode(repeat('01',n),'hex') FROM community_candidate_skins s CROSS JOIN generate_series(1,10) n`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "community_candidate_skins s", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	w := httptest.NewRecorder()
	a.adminList(w, httptest.NewRequest("GET", "/api/candidate-skins", nil), candidateSkinsList, 1, "")
	var result struct {
		Items []struct {
			Size      int64 `json:"size"`
			FileCount int   `json:"file_count"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != 50 || result.Total != 121 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, item := range result.Items {
		if item.Size != 55 || item.FileCount != 10 {
			t.Fatalf("文件统计错误：%+v", item)
		}
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到候选皮肤列表查询")
	}
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
	loops := 0
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "community_candidate_skin_files" {
			loops += n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if loops != 50 {
		t.Fatalf("文件统计执行了 %d 次，期望页内 50 次", loops)
	}
}
