package account

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type crashGroupsBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *crashGroupsBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (t *crashGroupsBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *crashGroupsBatchTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
}

func (*crashGroupsBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*crashGroupsBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminCrashGroupsBatchesItemsAndSummary(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,admin_crash_groups`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES('0123456789abcdef','windows','1.0','批量测试')`); err != nil {
		t.Fatal(err)
	}
	trace := &crashGroupsBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	getCrashGroups(t, a, "", 200)
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 3 {
		t.Fatalf("崩溃分组使用了 %d 次批量请求、%d 条语句，期望 1、3", batches, queries)
	}
}
