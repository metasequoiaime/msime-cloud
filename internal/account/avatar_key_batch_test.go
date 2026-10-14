package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type avatarKeyTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *avatarKeyTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*avatarKeyTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSetAvatarKeyReturnsPreviousKeyInOneWrite(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "avatar-key-batch@example.test"}).User.ID
	if _, err := db.pool.Exec(ctx, "UPDATE auth_users SET avatar_key='old-avatar' WHERE id=$1", user); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &avatarKeyTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	previous, err := (&Store{pool: pool}).SetAvatarKey(ctx, user, "new-avatar")
	if err != nil || previous != "old-avatar" {
		t.Fatalf("旧头像=%q 错误=%v", previous, err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 4 {
		t.Fatalf("头像替换应使用事务、用户锁和一次合并写入共 4 条语句，实际为 %d", queries)
	}
}
