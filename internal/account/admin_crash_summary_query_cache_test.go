package account

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCrashSummaryQueryReusesSQL(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	seedCrash(t, a, "query-reuse-macos", "Darwin", "1.0", "合成崩溃", "", "query-reuse-device-macos", time.Hour)
	seedCrash(t, a, "query-reuse-windows", "WIN", "1.0", "合成崩溃", "", "query-reuse-device-windows", time.Hour)
	trace := &communityPaginationTrace{table: "admin_events", queries: make(chan communityPageQuery, 16)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a = &Service{store: &Store{pool: pool}}
	var firstSQL string
	for _, tc := range []struct {
		query, platform string
		count           int64
	}{
		{"", "", 2}, {"?platform=Darwin", "macos", 1}, {"?platform=win", "windows", 1},
	} {
		body := getCrashGroups(t, a, tc.query, 200)
		if body.Summary.Groups != tc.count || body.Summary.Crashes7d != tc.count || len(body.Items) != int(tc.count) {
			t.Fatalf("崩溃汇总平台筛选错误：%+v", body)
		}
		var query communityPageQuery
		for len(trace.queries) > 0 {
			candidate := <-trace.queries
			if strings.Contains(candidate.sql, "::float8") && len(candidate.args) == 1 {
				query = candidate
			}
		}
		if query.sql == "" || len(query.args) != 1 || query.args[0] != tc.platform {
			t.Fatalf("汇总平台参数变化：%v", query.args)
		}
		if firstSQL == "" {
			firstSQL = query.sql
			continue
		}
		if query.sql != firstSQL {
			t.Fatal("固定崩溃汇总查询随平台变化")
		}
		// 保留前后查询并比较只读数据，检出逐请求拼接相同 SQL 的回归。
		if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
			t.Error("崩溃汇总查询逐请求分配 SQL，期望复用初始化结果")
		}
	}
}
