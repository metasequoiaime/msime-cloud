package account

import (
	"testing"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDownloadsTelemetryQueryReusesSQL(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,release_asset_snapshots`); err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_events(id,kind,platform,version,artifact,channel,created_at) VALUES
 ('query-reuse-first','download','windows','1.0','setup.exe','cn-mirror',$1),
 ('query-reuse-next','download',' win ','1.0','setup.exe','cn-mirror',$2)`, today.Add(time.Hour), today.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_events", queries: make(chan communityPageQuery, 4)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	var firstSQL string
	for day := 0; day < 2; day++ {
		start := today.AddDate(0, 0, day)
		summary, err := a.downloadsSummary(t.Context(), start.Add(10*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if summary.Day != start.Format(time.DateOnly) || summary.Totals.Today != 1 || summary.Totals.Week != int64(day+1) || len(summary.Rows) != 1 || summary.Rows[0].Platform != "windows" {
			t.Fatalf("下载汇总日期、窗口或平台归并错误：%+v", summary)
		}
		var query communityPageQuery
		select {
		case query = <-trace.queries:
		default:
			t.Fatal("没有记录到下载遥测查询")
		}
		if len(query.args) != 5 {
			t.Fatalf("查询参数个数变化：%d", len(query.args))
		}
		for i, want := range []time.Time{start, start.AddDate(0, 0, -6), start.AddDate(0, 0, 1)} {
			got, ok := query.args[i].(time.Time)
			if !ok || !got.Equal(want) {
				t.Fatalf("查询时间参数 %d 错误：%v", i, query.args[i])
			}
		}
		if query.args[3] != downloadsMirrorChannel || query.args[4] != downloadsRowLimit+1 {
			t.Fatal("下载渠道或行数限制变化")
		}
		if firstSQL == "" {
			firstSQL = query.sql
			continue
		}
		if query.sql != firstSQL {
			t.Fatal("固定下载查询文本随请求变化")
		}
		// 同时保留前后两次查询，直接检出重新构造的 SQL 字符串。
		if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
			t.Error("下载遥测查询逐请求分配 SQL，期望复用初始化结果")
		}
	}
}
