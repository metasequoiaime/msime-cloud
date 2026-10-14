package account

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminOverviewCrashCountsUseOneScan(t *testing.T) {
	a := overviewTestService(t)
	db := a.store
	overview := func(service *Service) (int, int) {
		t.Helper()
		w := httptest.NewRecorder()
		service.AdminHTTP(w, adminJSONRequest("GET", "/api/overview?days=7", ""))
		var body struct {
			Crashes     int `json:"crashes"`
			OpenCrashes int `json:"open_crashes"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("总览请求失败：%d %s", w.Code, w.Body.String())
		}
		return body.Crashes, body.OpenCrashes
	}
	if total, open := overview(a); total != 0 || open != 0 {
		t.Fatalf("空表统计错误：total=%d open=%d", total, open)
	}
	// 总数包含所有日期的崩溃，未解决数只按 resolved 区分；其他事件不参与。
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_events(id,kind,platform,version,resolved,created_at) VALUES
 ('overview-crash-old','crash','windows','1',false,now()-interval '90 days'),
 ('overview-crash-future','crash','windows','1',false,now()+interval '2 days'),
 ('overview-crash-fixed','crash','ios','1',true,now()),
 ('overview-crash-fixed-old','crash','ios','1',true,now()-interval '60 days'),
 ('overview-crash-download','download','windows','1',false,now()),
 ('overview-crash-session','session_crash','ios','1',false,now())`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_events", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if total, open := overview(&Service{store: &Store{pool: pool}}); total != 4 || open != 2 {
		t.Fatalf("崩溃统计错误：total=%d open=%d", total, open)
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到总览查询")
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation  string `json:"Relation Name"`
		Filter    string `json:"Filter"`
		IndexCond string `json:"Index Cond"`
		Recheck   string `json:"Recheck Cond"`
		Loops     int    `json:"Actual Loops"`
		Plans     []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	scans := 0
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "admin_events" && strings.Contains(n.Filter+n.IndexCond+n.Recheck, "kind = 'crash'::text") {
			scans += n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if scans != 1 {
		t.Fatalf("崩溃总数和未解决数扫描了 %d 次，期望 1 次", scans)
	}
}
