package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNotifyCrashSpikesUsesBoundedDatabaseCalls(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	resetCrashTables(t, &Service{store: db})
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
	a := &Service{store: &Store{pool: pool}}
	notify := func(wantCount, wantCalls int) {
		t.Helper()
		trace.mu.Lock()
		trace.seen, trace.statements = 0, nil
		trace.mu.Unlock()
		if count, err := a.NotifyCrashSpikes(ctx); err != nil || count != wantCount {
			t.Fatal(count, err)
		}
		trace.mu.Lock()
		calls := trace.seen
		trace.mu.Unlock()
		if calls != wantCalls {
			t.Errorf("崩溃告警用了 %d 次数据库调用，期望 %d 次", calls, wantCalls)
		}
	}
	notify(0, 3)
	signature := seedCrash(t, &Service{store: db}, "synthetic-spike-event", "ios", "1", "合成崩溃", "", "", time.Hour)
	notify(1, 4)
	notify(0, 3)
	var count int
	var title string
	if err := db.pool.QueryRow(ctx, `SELECT count(*),min(title) FROM admin_notifications WHERE kind='crash_spike' AND target_id=$1`, signature).Scan(&count, &title); err != nil || count != 1 || title != "iOS 新增崩溃分组 合成崩溃" {
		t.Fatal("崩溃告警重复或内容不正确", count, title, err)
	}
}

func TestNotifyCrashSpikesSeesNotificationCommittedDuringLockWait(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	signature := seedCrash(t, a, "synthetic-spike-event", "ios", "1", "合成崩溃", "", "", time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, crashSpikeLock); err != nil {
		t.Fatal(err)
	}
	type result struct {
		count int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		count, err := a.NotifyCrashSpikes(ctx)
		done <- result{count, err}
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	if _, err := blocker.Exec(ctx, insertNotification, NotifyCrashSpike, "首次告警", "crash", signature); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.err != nil || got.count != 0 {
		t.Fatal("未看到等待期间提交的告警", got)
	}
	var count int
	var title string
	if err := db.pool.QueryRow(ctx, `SELECT count(*),min(title) FROM admin_notifications WHERE kind='crash_spike' AND target_id=$1`, signature).Scan(&count, &title); err != nil || count != 1 || title != "首次告警" {
		t.Fatal("重复写入了崩溃告警", count, title, err)
	}
}

func TestNotifyCrashSpikesWriteFailureReleasesLock(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	resetCrashTables(t, a)
	seedCrash(t, a, "synthetic-spike-event", "ios", "1", "合成崩溃", "", "", time.Hour)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_notifications ADD CONSTRAINT crash_spike_test_reject CHECK(kind<>'crash_spike') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	drop := `ALTER TABLE admin_notifications DROP CONSTRAINT IF EXISTS crash_spike_test_reject`
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), drop); err != nil {
			t.Error(err)
		}
	})
	probe, err := db.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Release()
	var failure *pgconn.PgError
	if count, err := a.NotifyCrashSpikes(ctx); !errors.As(err, &failure) || failure.Code != "23514" || count != 0 {
		t.Fatal("崩溃告警写入失败未返回约束错误", count, err)
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_notifications WHERE kind='crash_spike'`); count != 0 {
		t.Fatal("写入失败仍保留了告警", count)
	}
	var acquired bool
	if err := probe.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, crashSpikeLock).Scan(&acquired); err != nil || !acquired {
		t.Fatal("写入失败未释放崩溃告警事务锁", acquired, err)
	}
	if _, err := db.pool.Exec(ctx, drop); err != nil {
		t.Fatal(err)
	}
	if count, err := a.NotifyCrashSpikes(ctx); err != nil || count != 1 {
		t.Fatal("写入失败后无法重试崩溃告警", count, err)
	}
}
