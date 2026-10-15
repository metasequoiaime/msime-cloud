package account

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

func TestDeleteUserDataUsesOneDatabaseBatch(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "delete-data-batch@example.test"}).User.ID
	cfg := db.pool.Config()
	trace := &pruneBatchTracer{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	if err = (&Store{pool: pool}).DeleteUserData(ctx, user, map[string]bool{
		"preferences": true,
		"dictionary":  true,
		"phrases":     true,
		"clipboard":   true,
		"voice":       true,
	}); err != nil {
		t.Fatal(err)
	}

	trace.mu.Lock()
	batches, queries := trace.batches, trace.queries
	trace.mu.Unlock()
	if batches != 1 {
		t.Fatalf("删除用户数据使用了 %d 次数据库批量请求，期望 1 次", batches)
	}
	if queries != 13 {
		t.Fatalf("删除用户数据批量请求包含 %d 条语句，期望 13 条", queries)
	}
}
