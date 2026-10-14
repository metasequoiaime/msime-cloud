package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type userMeBatchTrace struct {
	mu           sync.Mutex
	batches      int
	batchQueries int
	queries      int
}

func (t *userMeBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *userMeBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.batchQueries++
	t.mu.Unlock()
}

func (*userMeBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (t *userMeBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*userMeBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMeBatchesUserAndIdentityReads(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "user-me-batch@example.test"}).User.ID
	cfg := db.pool.Config()
	trace := &userMeBatchTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.batches, trace.batchQueries, trace.queries = 0, 0, 0
	trace.mu.Unlock()
	store := &Store{pool: pool}
	profile, identities, err := store.Me(ctx, user)
	if err != nil || profile.ID != user || len(identities) != 1 {
		t.Fatalf("资料=%+v 身份=%+v 错误=%v", profile, identities, err)
	}
	trace.mu.Lock()
	batches, batchQueries, queries := trace.batches, trace.batchQueries, trace.queries
	trace.mu.Unlock()
	if batches != 1 || batchQueries != 2 || queries != 0 {
		t.Fatalf("资料读取应使用 1 次批量请求和 2 条批量语句，实际为批量 %d/%d、普通查询 %d", batches, batchQueries, queries)
	}
}
