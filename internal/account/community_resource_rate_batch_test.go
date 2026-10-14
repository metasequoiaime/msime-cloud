package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resourceRateTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *resourceRateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*resourceRateTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestResourceRateOwnResourceUsesOneDatabaseRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "resource-rate-batch-owner@example.test"})
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('resource-rate-batch',$1,'reply','回复','{"prompt":"好的"}')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}

	cfg := db.pool.Config()
	trace := &resourceRateTrace{}
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
	apiRequest(t, mux, "PUT", "/v1/community/resources/resource-rate-batch/rating", `{"stars":4}`, owner.AccessToken, 403)

	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 3 {
		t.Fatalf("自己的作品评分应使用限流、鉴权和一次合并写入共 3 条语句，实际为 %d", queries)
	}
}
