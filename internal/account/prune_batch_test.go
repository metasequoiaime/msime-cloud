package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pruneBatchTracer struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *pruneBatchTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *pruneBatchTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*pruneBatchTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (t *pruneBatchTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*pruneBatchTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPruneUsesOneDatabaseBatch(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	cfg := db.pool.Config()
	trace := &pruneBatchTracer{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	(&Store{pool: pool}).Prune(ctx)

	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 {
		t.Fatalf("Prune 使用了 %d 次数据库批量请求，期望 1 次", batches)
	}
	if queries < 10 {
		t.Fatalf("Prune 批量请求只包含 %d 条清理语句，期望保留全部清理任务", queries)
	}
}
