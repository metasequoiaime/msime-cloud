package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type communitySkinCategoryBatchTrace struct {
	mu           sync.Mutex
	batches      int
	batchQueries int
}

func (t *communitySkinCategoryBatchTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	t.batches++
	t.mu.Unlock()
	return ctx
}

func (t *communitySkinCategoryBatchTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	t.mu.Lock()
	t.batchQueries++
	t.mu.Unlock()
}

func (*communitySkinCategoryBatchTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}
func (*communitySkinCategoryBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (*communitySkinCategoryBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestCommunitySkinCategoryUpdateBatchesWriteAndResponse(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "community-skin-category-batch@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,description,design) VALUES('community-skin-category-batch',$1,'分类测试','','`+communityFixture+`')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	trace := &communitySkinCategoryBatchTrace{}
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
	if w := apiRequest(t, mux, "PATCH", "/v1/community/skins/community-skin-category-batch?include=category", `{"category":"tech"}`, owner.AccessToken, 200); w.Code != 200 {
		t.Fatalf("分类更新失败：%d %s", w.Code, w.Body.String())
	}
	trace.mu.Lock()
	batches, queries := trace.batches, trace.batchQueries
	trace.mu.Unlock()
	if batches != 1 || queries != 2 {
		t.Fatalf("分类更新与响应读取使用了 %d 次批量请求、批内 %d 条语句，期望 1、2", batches, queries)
	}
}
