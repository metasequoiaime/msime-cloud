package account

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCrashIssueOperationsUseLockedPoolConnection(t *testing.T) {
	for _, operation := range []string{"read", "record"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			const signature = "abcdef0123456789"
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_crash_groups(signature,platform,version,title)
VALUES($1,'ios','1.0','合成崩溃') ON CONFLICT(signature) DO UPDATE SET status='open',issue_url=NULL`, signature); err != nil {
				t.Fatal(err)
			}
			var auditsBefore int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE action='crash_group_issue' AND target=$1`, signature).Scan(&auditsBefore); err != nil {
				t.Fatal(err)
			}
			cfg := db.pool.Config()
			// 一个连接等价于多个不同分组的 Issue 请求已经占满连接池。
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a := &Service{store: &Store{pool: pool}}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			locked, err := a.LockCrashGroupIssue(ctx, signature)
			if err != nil {
				t.Fatal(err)
			}
			defer locked.Release()
			if operation == "read" {
				source, err := locked.Source(ctx)
				if err != nil || source.Signature != signature || source.Platform != "ios" {
					t.Fatalf("持锁后重读分组阻塞或失败：source=%+v err=%v", source, err)
				}
			} else {
				if err := locked.Record(ctx, "synthetic/ios", 7, "https://example.test/issues/7"); err != nil {
					t.Fatalf("持锁后登记 Issue 阻塞或失败：%v", err)
				}
				var status, issueURL string
				var audits int
				if err := db.pool.QueryRow(t.Context(), `SELECT status,issue_url,
(SELECT count(*) FROM admin_audit WHERE action='crash_group_issue' AND target=$1)
FROM admin_crash_groups WHERE signature=$1`, signature).Scan(&status, &issueURL, &audits); err != nil || status != "known" || issueURL != "https://example.test/issues/7" || audits != auditsBefore+1 {
					t.Fatalf("Issue 或审计未写入：status=%s url=%s audits=%d err=%v", status, issueURL, audits, err)
				}
			}
			if ctx.Err() != nil {
				t.Fatal("Issue 操作耗尽请求时限", ctx.Err())
			}
		})
	}
}
