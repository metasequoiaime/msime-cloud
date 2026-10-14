package account

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminSessionTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *adminSessionTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*adminSessionTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminSessionTouchUsesOneDatabaseRoundTrip(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	token, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "session-batch", Email: "session-batch@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, "UPDATE admin_sessions SET last_seen_at=$2 WHERE token_hash=$1", hash(token), time.Now().Add(-adminSessionTouch-time.Second)); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &adminSessionTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	identity, err := (&Service{store: &Store{pool: pool}}).AdminSession(ctx, token)
	if err != nil || identity.Email != "session-batch@example.test" {
		t.Fatalf("身份=%+v 错误=%v", identity, err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("过期会话触摸应只执行 1 条语句，实际为 %d", queries)
	}
}
