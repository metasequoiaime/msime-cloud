package account

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminFilteredListStatsRunOnlyForPage(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE community_reports,community_skin_downloads,community_skins CASCADE`); err != nil {
		t.Fatal(err)
	}
	// 四种状态/分类组合各 121 条，同时筛选后应只剩 pending+tech 的一组。
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id,display_name) SELECT 'filtered-owner-'||n,'合成作者' FROM generate_series(0,24) n;
 INSERT INTO auth_users(id) VALUES('filtered-reader-1'),('filtered-reader-2');
 INSERT INTO community_skins(id,owner_id,name,design,moderation,category,created_at)
 SELECT 'filtered-skin-'||n,'filtered-owner-'||((n-1)/20),'合成皮肤','{}',CASE WHEN n%4 IN (0,1) THEN 'pending' ELSE 'approved' END,CASE WHEN n%2=0 THEN 'tech' ELSE 'nature' END,now()+n*interval '1 second' FROM generate_series(1,484) n;
 INSERT INTO community_skin_downloads(skin_id,user_id) SELECT s.id,'filtered-reader-'||n FROM community_skins s CROSS JOIN generate_series(1,2) n;
 INSERT INTO community_reports(kind,item_id,reporter_id,reason) SELECT 'skins',id,'filtered-reader-1','other' FROM community_skins;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "community_skins s", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for page := 1; page <= 4; page++ {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			w := httptest.NewRecorder()
			a.adminList(w, httptest.NewRequest("GET", "/api/skins?status=pending&category=tech", nil), skinsList, page, "")
			var result struct {
				Items []struct {
					ID         string `json:"id"`
					Downloads  int    `json:"downloads"`
					Reports    int    `json:"reports"`
					Moderation string `json:"moderation"`
					Category   string `json:"category"`
				} `json:"items"`
				Total   int  `json:"total"`
				Page    int  `json:"page"`
				HasMore bool `json:"has_more"`
			}
			wantRows := max(0, min(50, 121-(page-1)*50))
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != wantRows || result.Total != 121 || result.Page != page || result.HasMore != (page < 3) {
				t.Fatal(w.Code, w.Body.String())
			}
			for i, item := range result.Items {
				wantID := fmt.Sprintf("filtered-skin-%d", 484-(page-1)*200-i*4)
				if item.ID != wantID || item.Downloads != 2 || item.Reports != 1 || item.Moderation != "pending" || item.Category != "tech" {
					t.Fatalf("筛选后的条目错误：%+v，期望 %s", item, wantID)
				}
			}
			var query communityPageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到筛选列表查询")
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
			loops := map[string]int{}
			var visit func(node)
			visit = func(n node) {
				loops[n.Relation] += n.Loops
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			for _, relation := range []string{"community_skin_downloads", "community_reports"} {
				if loops[relation] != wantRows {
					t.Errorf("筛选列表对 %s 执行了 %d 次统计，期望页内 %d 次", relation, loops[relation], wantRows)
				}
			}
		})
	}
}
