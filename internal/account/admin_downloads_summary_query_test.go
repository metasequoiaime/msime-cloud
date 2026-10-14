package account

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type downloadSummaryTrace struct {
	queries []string
}

func (trace *downloadSummaryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.capture(data.SQL)
	return ctx
}

func (trace *downloadSummaryTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (trace *downloadSummaryTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	trace.capture(data.SQL)
}

func (trace *downloadSummaryTrace) capture(sql string) {
	if strings.Contains(sql, "FROM admin_events") {
		trace.queries = append(trace.queries, sql)
	}
}

func (*downloadSummaryTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*downloadSummaryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDownloadsSummaryScansTelemetryOnce(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_events,release_asset_snapshots`); err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	// 前 500 组各有两次下载；仅在截断后的最后一组上报渠道，汇总仍须包含它。
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,channel,created_at)
 SELECT 'summary-cap-'||n||'-'||copy,'download','windows','v'||n,NULL,$1::timestamptz
 FROM generate_series(1,500) n CROSS JOIN generate_series(1,2) copy
 UNION ALL SELECT 'summary-cap-mirror','download','windows','v-last','cn-mirror',$1::timestamptz-interval '6 days'`, today); err != nil {
		t.Fatal(err)
	}
	trace := &downloadSummaryTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	summary, err := a.downloadsSummary(ctx, today)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Rows) != downloadsRowLimit || !summary.Truncated {
		t.Fatalf("分组截断错误：行数=%d，truncated=%v", len(summary.Rows), summary.Truncated)
	}
	if summary.Totals != (downloadsTotals{Today: 1000, Week: 1001, MirrorWeek: 1}) || !summary.ChannelReported || summary.MirrorShare == nil || math.Abs(*summary.MirrorShare-1.0/1001) > 1e-9 {
		t.Fatalf("汇总遗漏截断后的分组：%+v", summary)
	}
	if len(trace.queries) != 1 {
		t.Fatalf("下载汇总应只查询一次事件表，实际为 %d 次", len(trace.queries))
	}
}
