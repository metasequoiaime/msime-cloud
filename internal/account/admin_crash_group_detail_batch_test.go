package account

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type crashGroupDetailBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *crashGroupDetailBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (t *crashGroupDetailBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *crashGroupDetailBatchTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT") || strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
}

func (*crashGroupDetailBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (*crashGroupDetailBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminCrashGroupBatchesGroupAndSamples(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events,admin_crash_groups`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES('fedcba9876543210','windows','1.0','批量测试'),('0123456789abcdef','windows','1.0','其他分组')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_events(id,kind,platform,version,message,stack,signature) VALUES('crash-detail-batch','crash','windows','1.0','测试','堆栈','fedcba9876543210')`); err != nil {
		t.Fatal(err)
	}
	trace := &crashGroupDetailBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	getCrashGroups(t, a, "/fedcba9876543210", 200)
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 2 {
		t.Fatalf("崩溃分组详情使用了 %d 次批量请求、%d 条语句，期望 1、2", batches, queries)
	}
}
