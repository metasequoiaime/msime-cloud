package account

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type downloadsSummaryBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *downloadsSummaryBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (t *downloadsSummaryBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *downloadsSummaryBatchTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
}

func (*downloadsSummaryBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*downloadsSummaryBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDownloadsSummaryBatchesTelemetryAndSnapshotDay(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,release_asset_snapshots`); err != nil {
		t.Fatal(err)
	}
	trace := &downloadsSummaryBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	if _, err := a.downloadsSummary(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 3 {
		t.Fatalf("下载汇总使用了 %d 次批量请求、%d 条语句，期望 1、3", batches, queries)
	}
}
