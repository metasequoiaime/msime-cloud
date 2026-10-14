package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communitySaveBatchTrace struct {
	mu      sync.Mutex
	batches int
	queries int
}

func (t *communitySaveBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *communitySaveBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
}

func (*communitySaveBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (t *communitySaveBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*communitySaveBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunitySaveBatchesMutationAndCount(t *testing.T) {
	db := testStore(t)
	reader := complete(t, db, Identity{"email", "community-save-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('community-save-batch-skin',$1,'收藏测试','{}')`, reader.User.ID); err != nil {
		t.Fatal(err)
	}
	trace := &communitySaveBatchTrace{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	mux := http.NewServeMux()
	Mount(mux, a)
	if w := apiRequest(t, mux, "PUT", "/v1/community/skins/community-save-batch-skin/save", `{"saved":true}`, reader.AccessToken, 200); w.Code != 200 {
		t.Fatalf("收藏请求失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 || queries != 2 {
		t.Fatalf("收藏写入使用了 %d 次批量请求、%d 条语句，期望 1、2", batches, queries)
	}
}
