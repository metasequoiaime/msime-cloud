package account

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminSessionCreateTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *adminSessionCreateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries++
	t.mu.Unlock()
	return ctx
}

func (*adminSessionCreateTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCreateAdminSessionChecksMembershipAndInsertsInOneQuery(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	email := "session-batch-" + randomToken()[:8] + "@example.test"
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email,role) VALUES($1,'reviewer')`, email); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &adminSessionCreateTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.queries = 0
	trace.mu.Unlock()
	_, err = (&Service{store: &Store{pool: pool}}).CreateAdminSession(ctx, AdminIdentity{Subject: "session-batch", Email: email, Name: "管理员"})
	if err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 3 {
		t.Fatalf("管理员会话创建使用了 %d 条数据库语句，期望事务、合并检查写入、提交共 3 条", queries)
	}
}
