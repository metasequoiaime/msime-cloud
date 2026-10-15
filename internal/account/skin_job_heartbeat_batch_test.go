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

type skinJobHeartbeatBatchTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *skinJobHeartbeatBatchTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "skin_jobs") {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}

func (*skinJobHeartbeatBatchTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSkinJobHeartbeatUsesOneDatabaseStatement(t *testing.T) {
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
	trace := &skinJobHeartbeatBatchTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	traced := &Service{store: &Store{pool: pool}}
	alive, err := traced.SkinJobHeartbeat(ctx, jobID)
	if err != nil || !alive {
		t.Fatal(alive, err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("皮肤任务心跳使用了 %d 条数据库语句，期望 1 条", queries)
	}
}
