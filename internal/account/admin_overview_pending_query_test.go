package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type overviewQueryCountTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *overviewQueryCountTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*overviewQueryCountTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminOverviewBatchesPendingCount(t *testing.T) {
	db := testStore(t)
	trace := &overviewQueryCountTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	if body := getOverview(t, a, "?days=7"); body.RangeDays != 7 {
		t.Fatalf("总览请求返回天数=%d", body.RangeDays)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("总览读取使用了 %d 条数据库语句，期望 1 条", queries)
	}
}
