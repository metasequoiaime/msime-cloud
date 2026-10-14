package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminMePersonalBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *adminMePersonalBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *adminMePersonalBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*adminMePersonalBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (t *adminMePersonalBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*adminMePersonalBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminMeBatchesPersonalReads(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_members(email,role) VALUES('batch-personal@example.test','reviewer')`); err != nil {
		t.Fatal(err)
	}
	seed := &Service{store: db}
	current, err := seed.CreateAdminSession(t.Context(), AdminIdentity{Subject: "batch-personal", Email: "batch-personal@example.test", UserAgent: testSafariUA})
	if err != nil {
		t.Fatal(err)
	}
	trace := &adminMePersonalBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	access := AdminAccess{Actor: "google:batch-personal:batch-personal@example.test", Email: "batch-personal@example.test", Role: "reviewer", Permissions: []string{PermReviewCommunity}}
	if w := meRequest(a, access, "GET", "", current); w.Code != 200 {
		t.Fatalf("个人中心请求失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 2 || queries != 7 {
		t.Fatalf("个人中心读取使用了 %d 次批量请求、%d 条语句，期望 2、7", batches, queries)
	}
}
