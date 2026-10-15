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

type skinJobDeleteBatchTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *skinJobDeleteBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "skin_jobs") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (*skinJobDeleteBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDeleteSkinJobUsesOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	jobID := skinJobID('a')
	service := &Service{store: db}
	if _, err := service.CreateSkinJob(ctx, jobID, "owner", 1, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &skinJobDeleteBatchTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	traced := &Service{store: &Store{pool: pool}}
	if err := traced.DeleteSkinJob(ctx, jobID, "owner"); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("删除皮肤任务使用了 %d 条数据库语句，期望 1 条", queries)
	}
}
