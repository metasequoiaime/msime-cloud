package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type skinPublishTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *skinPublishTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*skinPublishTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunitySkinPublishQuotaInsertUsesOneDatabaseRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "skin-publish-batch@example.test"})
	cfg := db.pool.Config()
	trace := &skinPublishTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: &Store{pool: pool}}
	if _, err = service.Sensitive().Match(ctx, "干净内容"); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	mux := http.NewServeMux()
	Mount(mux, service)
	apiRequest(t, mux, "POST", "/v1/community/skins", `{"id":"ab334455-1234-1234-1234-123456789abe","name":"测试皮肤","description":"","design":`+communityFixture+`}`, owner.AccessToken, 201)

	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 7 {
		t.Fatalf("皮肤首发应使用 7 条语句，实际为 %d", queries)
	}
}
