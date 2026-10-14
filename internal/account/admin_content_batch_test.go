package account

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminContentBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *adminContentBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *adminContentBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*adminContentBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (t *adminContentBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*adminContentBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminContentBatchesModerationReads(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "admin-content-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,description,design) VALUES('admin-content-batch-skin',$1,'批量读取','','{}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	trace := &adminContentBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	w := httptest.NewRecorder()
	a.adminContent(w, httptest.NewRequest("GET", "/api/skins/admin-content-batch-skin", nil), "skins", "admin-content-batch-skin")
	if w.Code != 200 {
		t.Fatalf("内容详情请求失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 3 {
		t.Fatalf("内容审核信息使用了 %d 次批量请求、%d 条语句，期望 1、3", batches, queries)
	}
}
