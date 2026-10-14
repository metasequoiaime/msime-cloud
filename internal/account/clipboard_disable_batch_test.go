package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type clipboardDisableBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *clipboardDisableBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}
func (t *clipboardDisableBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}
func (t *clipboardDisableBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}
func (*clipboardDisableBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*clipboardDisableBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSetClipboardEnabledBatchesDisableCleanup(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "clipboard-disable-batch@example.test"}).User.ID
	trace := &clipboardDisableBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Store{pool: pool}
	if err := s.SetClipboardEnabled(t.Context(), user, false); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 6 {
		t.Fatalf("关闭剪贴板使用了 %d 次批量请求、%d 条语句，期望 1、6", batches, queries)
	}
}
