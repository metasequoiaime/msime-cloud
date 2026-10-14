package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminMeBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *adminMeBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *adminMeBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*adminMeBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (t *adminMeBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*adminMeBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminMeBatchesStatsAndRecentAudit(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	trace := &adminMeBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	legacy := AdminAccess{Actor: "legacy-token", Role: RoleMaintainer, Permissions: AllAdminPermissions()}
	if w := meRequest(a, legacy, "GET", "", ""); w.Code != 200 {
		t.Fatalf("个人中心请求失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 2 {
		t.Fatalf("个人中心统计使用了 %d 次批量请求、%d 条语句，期望 1、2", batches, queries)
	}
}
