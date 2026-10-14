package account

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type skinRateTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *skinRateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*skinRateTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommunitySkinRateUsesOneWriteQueryOnOwnSkin(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"anonymous", "skin-rate-batch"})
	id := "01234567-1234-4234-8234-012345678901"
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'皮肤', $3)`, id, owner.User.ID, communityFixture); err != nil {
		t.Fatal(err)
	}

	trace := &skinRateTrace{}
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
	apiRequest(t, mux, "PUT", "/v1/community/skins/"+id+"/rating", `{"stars":5}`, owner.AccessToken, 403)
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 6 {
		t.Fatalf("给自己的皮肤评分使用了 %d 条数据库语句，期望包含用户行锁事务的固定查询数", queries)
	}
}
