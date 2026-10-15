package account

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReleaseDownloadsBaselineLimitsHistoricalRows(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	today := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if _, err := db.pool.Exec(ctx, `TRUNCATE release_asset_snapshots;
 INSERT INTO release_asset_snapshots(repo,tag,asset,day,download_count)
 SELECT 'owner/example','windows-v1','asset-'||n||'.exe',date '2026-10-14'-age,1000-age
 FROM generate_series(1,20) n CROSS JOIN generate_series(8,127) age;
 INSERT INTO release_asset_snapshots(repo,tag,asset,day,download_count) VALUES
 ('owner/example','windows-v1','asset-1.exe','2026-10-07',993),
 ('owner/example','windows-v1','asset-1.exe','2026-10-08',994),
 ('owner/example','windows-v1','asset-1.exe','2026-10-12',998),
 ('owner/example','windows-v1','asset-1.exe','2026-10-14',1000),
 ('owner/example','windows-v1','asset-1.exe','2026-10-15',2000),
 ('owner/example','windows-v1','asset-2.exe','2026-10-08',10),
 ('owner/example','windows-v1','asset-2.exe','2026-10-14',15),
 ('owner/example','windows-v1','new.exe','2026-10-12',20),
 ('owner/example','windows-v1','new.exe','2026-10-14',25),
 ('owner/example','windows-v1','asset-4.exe','2026-10-14',992),
 ('owner/example','windows-v1','latest.yml','2026-10-01',100),
 ('owner/example','windows-v1','latest.yml','2026-10-14',200),
 ('other/example','windows-v1','asset-1.exe','2026-10-01',50),
 ('other/example','windows-v1','asset-1.exe','2026-10-14',60),
 ('owner/example','windows-v2','asset-1.exe','2026-10-01',200),
 ('owner/example','windows-v2','asset-1.exe','2026-10-14',207);
 ANALYZE release_asset_snapshots;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "release_asset_snapshots", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var summary downloadsSummary
	dbRows, err := tx.Query(ctx, githubReleaseDownloadsQuery, today.AddDate(0, 0, -6), today)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := collectGitHubReleaseDownloads(dbRows, &summary)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int64{}
	for _, row := range rows {
		got[row.Repo+"/"+row.Tag+"/"+*row.Artifact] = [2]int64{row.Today, row.Week}
	}
	want := map[string][2]int64{
		"owner/example/windows-v1/asset-1.exe": {2, 7},
		"owner/example/windows-v1/asset-2.exe": {5, 5},
		"owner/example/windows-v1/new.exe":     {5, 5},
		"other/example/windows-v1/asset-1.exe": {10, 10},
		"owner/example/windows-v2/asset-1.exe": {7, 7},
	}
	if !maps.Equal(got, want) || len(rows) != len(want) || summary.Totals.GitHubToday != 29 || summary.Totals.GitHubWeek != 34 || summary.Truncated {
		t.Fatalf("快照基线或下载汇总错误：rows=%v totals=%+v", got, summary)
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到快照差分查询")
	}
	var raw json.RawMessage
	// 使用未加追踪的连接检查执行计划，避免把 EXPLAIN 再写入同一查询通道。
	if err := db.pool.QueryRow(ctx, `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Rows     int    `json:"Actual Rows"`
		Loops    int    `json:"Actual Loops"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	returnedRows := 0
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "release_asset_snapshots" {
			returnedRows += n.Rows * n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	// 本周只有 11 条快照、7 个资产，每个资产最多补一条基线，不应把 2400 多条历史行送入后续处理。
	if returnedRows == 0 || returnedRows > 18 {
		t.Fatalf("快照查询向后续节点输出 %d 行，期望最多 18 行", returnedRows)
	}
}
