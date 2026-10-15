package account

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type crashIssueWriteTrace struct {
	mu      sync.Mutex
	queries int
}

func (t *crashIssueWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL != "begin" && data.SQL != "commit" && data.SQL != "rollback" {
		t.mu.Lock()
		t.queries++
		t.mu.Unlock()
	}
	return ctx
}
func (*crashIssueWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSetCrashGroupIssueUsesOneDatabaseStatement(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES('abcdef0123456789','ios','1.0','测试') ON CONFLICT(signature) DO UPDATE SET status='open',issue_url=NULL`); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	trace := &crashIssueWriteTrace{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	ctx = WithAdminAccess(ctx, AdminAccess{Actor: "legacy-token"})
	if err := a.SetCrashGroupIssue(ctx, "abcdef0123456789", "repo/name", 7, "https://example.com/7"); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.queries
	trace.mu.Unlock()
	if queries != 1 {
		t.Fatalf("崩溃分组状态和审计写入使用了 %d 条数据库语句，期望 1 条", queries)
	}
	var detail string
	if err := db.pool.QueryRow(ctx, `SELECT detail::text FROM admin_audit WHERE action='crash_group_issue' AND target='abcdef0123456789'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(detail), &value); err != nil || value["number"] != float64(7) {
		t.Fatal(detail, err)
	}
	if err := a.SetCrashGroupIssue(ctx, "missing0123456789", "r/r", 1, "https://x"); !errors.Is(err, ErrCrashGroupNotFound) {
		t.Fatal(err)
	}
}
