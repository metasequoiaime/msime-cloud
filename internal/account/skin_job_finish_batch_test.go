package account

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type skinJobFinishBatchTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *skinJobFinishBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "skin_jobs") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (*skinJobFinishBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestFinishSkinJobUsesOneDatabaseStatement(t *testing.T) {
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
	trace := &skinJobFinishBatchTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	traced := &Service{store: &Store{pool: pool}}
	recorded, err := traced.FinishSkinJob(ctx, jobID, "succeeded", "", []byte(`{"width":1}`))
	if err != nil || !recorded {
		t.Fatal(recorded, err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("皮肤任务完成使用了 %d 条数据库语句，期望 1 条", queries)
	}
}
