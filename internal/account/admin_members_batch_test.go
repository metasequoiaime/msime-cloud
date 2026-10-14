package account

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminMemberCleanupBatchTrace struct {
	mu      sync.Mutex
	batches int
}

func (t *adminMemberCleanupBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}
func (*adminMemberCleanupBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}
func (*adminMemberCleanupBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}
func (*adminMemberCleanupBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*adminMemberCleanupBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestAdminMemberCleanupBatchesSessionAndTokenDeletes(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_members,admin_sessions,admin_tokens,admin_audit CASCADE`); err != nil {
		t.Fatal(err)
	}
	trace := &adminMemberCleanupBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	body := `{"action":"add","email":"batch-admin@example.test"}`
	r := jsonRequest("POST", "/api/admins", body, "").WithContext(WithAdminActor(t.Context(), "google:owner:owner@example.test"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
	if w.Code != 200 {
		t.Fatalf("添加成员失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches := trace.batches
	trace.mu.Unlock()
	if batches != 1 {
		t.Fatalf("成员凭据清理使用了 %d 次批量请求，期望 1", batches)
	}
}
