package account

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSetCrashGroupIssueUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES('abcdef0123456789','ios','1.0','测试') ON CONFLICT(signature) DO UPDATE SET status='open',issue_url=NULL`); err != nil {
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
	a := &Service{store: &Store{pool: pool}}
	ctx = WithAdminAccess(ctx, AdminAccess{Actor: "legacy-token"})
	if err := a.SetCrashGroupIssue(ctx, "abcdef0123456789", "repo/name", 7, "https://example.com/7"); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries := trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("崩溃分组状态和审计写入使用了 %d 次数据库调用，期望 1 次", queries)
	}
	var detail string
	if err := db.pool.QueryRow(ctx, `SELECT detail::text FROM admin_audit WHERE action='crash_group_issue' AND target='abcdef0123456789'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(detail), &value); err != nil || value["number"] != float64(7) || value["from"] != "open" || value["to"] != "known" || value["repo"] != "repo/name" || value["issue_url"] != "https://example.com/7" {
		t.Fatal(detail, err)
	}
	trace.mu.Lock()
	trace.seen, trace.statements = 0, nil
	trace.mu.Unlock()
	if err := a.SetCrashGroupIssue(ctx, "missing0123456789", "r/r", 1, "https://x"); !errors.Is(err, ErrCrashGroupNotFound) {
		t.Fatal(err)
	}
	trace.mu.Lock()
	queries = trace.seen
	trace.mu.Unlock()
	if queries != 1 {
		t.Errorf("不存在的崩溃分组用了 %d 次数据库调用，期望 1 次", queries)
	}
}

func TestSetCrashGroupIssueRollsBackWhenAuditFails(t *testing.T) {
	db := testStore(t)
	ctx := WithAdminAccess(t.Context(), AdminAccess{Actor: "legacy-token"})
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,issue_url)
VALUES('abcdef0123456789','ios','1.0','合成崩溃','fixed','https://example.test/old')
ON CONFLICT(signature) DO UPDATE SET status='fixed',issue_url='https://example.test/old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_audit ADD CONSTRAINT crash_issue_test_reject CHECK(action<>'crash_group_issue') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT IF EXISTS crash_issue_test_reject`); err != nil {
			t.Error(err)
		}
	})
	if err := (&Service{store: db}).SetCrashGroupIssue(ctx, "abcdef0123456789", "repo/name", 9, "https://example.test/9"); err == nil {
		t.Fatal("审计失败时 issue 登记返回成功")
	}
	var status, url string
	if err := db.pool.QueryRow(ctx, `SELECT status,issue_url FROM admin_crash_groups WHERE signature='abcdef0123456789'`).Scan(&status, &url); err != nil || status != "fixed" || url != "https://example.test/old" {
		t.Fatal("审计失败后分组状态或 issue 链接改变", status, url, err)
	}
	if n := clipboardTestCount(t, db, `SELECT count(*) FROM admin_audit WHERE action='crash_group_issue'`); n != 0 {
		t.Fatal("审计失败仍留下审计记录", n)
	}
}
