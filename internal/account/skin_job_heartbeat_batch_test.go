package account

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSkinJobHeartbeatUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	jobID := skinJobID('b')
	service := &Service{store: db}
	if _, err := service.CreateSkinJob(ctx, jobID, "owner", 1, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &statementCancellation{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	traced := &Service{store: &Store{pool: pool}}
	alive, err := traced.SkinJobHeartbeat(ctx, jobID)
	if err != nil || !alive {
		t.Fatal(alive, err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("皮肤任务心跳使用了 %d 次数据库调用，期望 1 次", queries)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if alive, err := traced.SkinJobHeartbeat(ctx, skinJobID('z')); err != nil || alive {
		t.Fatal("不存在的任务仍允许继续执行", alive, err)
	}
	trace.mu.Lock()
	queries = trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("不存在任务的心跳使用了 %d 次数据库调用，期望 1 次", queries)
	}
}
