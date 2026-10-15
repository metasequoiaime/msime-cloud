package account

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFinishSkinJobUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	jobID := skinJobID('f')
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
	recorded, err := traced.FinishSkinJob(ctx, jobID, "succeeded", "", []byte(`{"width":1}`))
	if err != nil || !recorded {
		t.Fatal(recorded, err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("皮肤任务完成使用了 %d 次数据库调用，期望 1 次", queries)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if recorded, err := traced.FinishSkinJob(ctx, jobID, "failed", "cancelled", nil); err != nil || recorded {
		t.Fatal("已完成任务仍接受结果", recorded, err)
	}
	trace.mu.Lock()
	queries = trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("拒绝覆盖已完成任务使用了 %d 次数据库调用，期望 1 次", queries)
	}
	if job, err := service.GetSkinJob(ctx, jobID, "owner"); err != nil || job.State != "succeeded" || job.Reason != "" || string(job.Artwork) != `{"width":1}` {
		t.Fatal("重复完成改变了原结果", job, err)
	}
}
