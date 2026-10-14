package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resourcePublishTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *resourcePublishTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*resourcePublishTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestResourcePublishQuotaInsertUsesOneDatabaseRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "resource-publish-batch@example.test"})
	cfg := db.pool.Config()
	trace := &resourcePublishTrace{}
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
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"ab334455-1234-1234-1234-123456789abd","kind":"reply","name":"回复","description":"","content":{"prompt":"好的"},"revision":0}`, owner.AccessToken, 201)

	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 8 {
		t.Fatalf("资源首发应使用 8 条语句，实际为 %d", queries)
	}
}
