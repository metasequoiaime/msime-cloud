package account

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeleteSkinJobUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	jobID := skinJobID('a')
	service := &Service{store: db}
	if _, _, err := service.CreateSkinJob(ctx, jobID, "owner", 1, 1, 10, time.Minute); err != nil {
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
	if err := traced.DeleteSkinJob(ctx, jobID, "owner"); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("删除皮肤任务使用了 %d 次数据库调用，期望 1 次", queries)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if err := traced.DeleteSkinJob(ctx, jobID, "owner"); !errors.Is(err, ErrSkinJobNotFound) {
		t.Fatal("重复删除未返回任务不存在", err)
	}
	trace.mu.Lock()
	queries = trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("重复删除任务使用了 %d 次数据库调用，期望 1 次", queries)
	}
}
