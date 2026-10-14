package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resourceSaveTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *resourceSaveTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*resourceSaveTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestResourceSaveExistingUsesOneDatabaseRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "resource-save-batch-owner@example.test"})
	reader := complete(t, db, Identity{"email", "resource-save-batch-reader@example.test"})
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('resource-save-batch',$1,'reply','回复','{"prompt":"好的"}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_resource_saves(resource_id,user_id) VALUES('resource-save-batch',$1)`, reader.User.ID); err != nil {
		t.Fatal(err)
	}

	cfg := db.pool.Config()
	trace := &resourceSaveTrace{}
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
	trace.queries = 0
	trace.mu.Unlock()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: &Store{pool: pool}})
	apiRequest(t, mux, "PUT", "/v1/community/resources/resource-save-batch/save", `{"saved":true}`, reader.AccessToken, 200)

	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 6 {
		t.Fatalf("重复收藏应使用包含用户行锁事务的固定查询数，实际为 %d", queries)
	}
}
