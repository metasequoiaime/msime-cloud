package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type candidateVisibilityTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *candidateVisibilityTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*candidateVisibilityTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunityCandidateVisibilityUsesOneQuotaQuery(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"anonymous", "candidate-visibility-batch"})
	id := "01234567-1234-4234-8234-012345678901"
	insertCandidateSkin(t, db, id, owner.User.ID, "候选皮肤")
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_candidate_skins SET visibility='private' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	trace := &candidateVisibilityTrace{}
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
	apiRequest(t, mux, "PATCH", "/v1/community/candidate-skins/"+id, `{"visibility":"public"}`, owner.AccessToken, 200)
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 9 {
		t.Fatalf("候选皮肤公开名额检查使用了 %d 条数据库语句，期望减少一次配额查询", queries)
	}
}
