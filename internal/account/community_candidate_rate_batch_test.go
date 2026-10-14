package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type candidateRateTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *candidateRateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*candidateRateTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunityCandidateRateUsesOneWriteQueryOnOwnSkin(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"anonymous", "candidate-rate-batch"})
	id := "01234567-1234-4234-8234-012345678901"
	insertCandidateSkin(t, db, id, owner.User.ID, "候选皮肤")

	trace := &candidateRateTrace{}
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
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	apiRequest(t, mux, "PUT", "/v1/community/candidate-skins/"+id+"/rating", `{"stars":5}`, owner.AccessToken, 403)
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 3 {
		t.Fatalf("给自己的候选皮肤评分使用了 %d 条数据库语句，期望路由限流、鉴权和写入各 1 条", queries)
	}
}
