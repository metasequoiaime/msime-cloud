package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNotifyOnceUsesOneDatabaseCall(t *testing.T) {
	a := notificationTestService(t)
	db, ctx := a.store, t.Context()
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	a = &Service{store: &Store{pool: pool}}
	for _, title := range []string{"首次通知", "重复通知"} {
		trace.mu.Lock()
		trace.seen, trace.statements = 0, nil
		trace.mu.Unlock()
		if err := a.NotifyOnce(ctx, Notification{Kind: NotifyDictPR, Title: title, TargetPage: "dictpr", TargetID: "synthetic-target"}); err != nil {
			t.Fatal(err)
		}
		trace.mu.Lock()
		calls := trace.seen
		trace.mu.Unlock()
		if calls != 1 {
			t.Errorf("通知去重用了 %d 次数据库调用，期望 1 次", calls)
		}
	}
	var count int
	var title string
	if err := db.pool.QueryRow(ctx, `SELECT count(*),min(title) FROM admin_notifications WHERE kind='dict_pr' AND target_id='synthetic-target'`).Scan(&count, &title); err != nil || count != 1 || title != "首次通知" {
		t.Fatal("重复调用改变了首次通知", count, title, err)
	}
}

func TestNotifyOnceSeesNotificationCommittedDuringLockWait(t *testing.T) {
	a := notificationTestService(t)
	db := a.store
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	n := Notification{Kind: NotifyDictPR, Title: "后续通知", TargetPage: "dictpr", TargetID: "synthetic-target"}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('admin_notifications:'||$1||':'||$2,0))`, n.Kind, n.TargetID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.NotifyOnce(ctx, n) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if _, err := blocker.Exec(ctx, insertNotification, n.Kind, "首次通知", n.TargetPage, n.TargetID); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	var title string
	if err := db.pool.QueryRow(ctx, `SELECT count(*),min(title) FROM admin_notifications WHERE kind=$1 AND target_id=$2`, n.Kind, n.TargetID).Scan(&count, &title); err != nil || count != 1 || title != "首次通知" {
		t.Fatal("等待锁期间提交的通知未被去重", count, title, err)
	}
}

func TestNotifyOnceReportsWriteFailureAndReleasesLock(t *testing.T) {
	a := notificationTestService(t)
	db, ctx := a.store, t.Context()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_notifications ADD CONSTRAINT notification_once_test_reject CHECK(kind<>'dict_pr') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_notifications DROP CONSTRAINT IF EXISTS notification_once_test_reject`); err != nil {
			t.Error(err)
		}
	})
	n := Notification{Kind: NotifyDictPR, Title: "合成通知", TargetPage: "dictpr", TargetID: "synthetic-target"}
	probe, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Release()
	var failure *pgconn.PgError
	if err := a.NotifyOnce(ctx, n); !errors.As(err, &failure) || failure.Code != "23514" {
		t.Fatal("通知写入失败未返回约束错误", err)
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_notifications`); count != 0 {
		t.Fatal("失败通知仍被写入", count)
	}
	var acquired bool
	if err := probe.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('admin_notifications:'||$1||':'||$2,0))`, n.Kind, n.TargetID).Scan(&acquired); err != nil || !acquired {
		t.Fatal("通知写入失败未释放事务锁", acquired, err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_notifications DROP CONSTRAINT notification_once_test_reject`); err != nil {
		t.Fatal(err)
	}
	if err := a.NotifyOnce(ctx, n); err != nil {
		t.Fatal("失败后无法重试通知", err)
	}
}
