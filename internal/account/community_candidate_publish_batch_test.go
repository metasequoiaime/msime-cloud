package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type candidatePublishTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *candidatePublishTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*candidatePublishTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCandidateSkinPublishQuotaInsertUsesOneDatabaseRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	owner := complete(t, db, Identity{"email", "candidate-publish-batch@example.test"})
	cfg := db.pool.Config()
	trace := &candidatePublishTrace{}
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
	manifest, files := candidateFixture(t, "candidate-batch")
	mux := http.NewServeMux()
	Mount(mux, service)
	apiRequest(t, mux, "POST", "/v1/community/candidate-skins", candidatePublishBody(t, "ab334455-1234-1234-1234-123456789ab0", "共享皮肤", manifest, files), owner.AccessToken, 201)

	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 11 {
		t.Fatalf("候选皮肤首发应使用 11 条语句，实际为 %d", queries)
	}
}
