package account

import (
	"errors"
	"testing"
	"time"
)

func TestRateTxUsesWallClockAfterAuthRateLockWait(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	key := "rate-tx-clock-" + randomToken()
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,1,clock_timestamp()+interval '500 milliseconds')`, key); err != nil {
		t.Fatal(err)
	}

	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, `SELECT key FROM auth_rates WHERE key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// 确保事务时钟在等待行锁前已经固定，回归旧实现使用 now() 的问题。
	if _, err = tx.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- rateTx(ctx, tx, key, 1, time.Minute) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	time.Sleep(700 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if errors.Is(err, ErrLimited) {
			t.Fatal("限流窗口已过期却仍被拒绝")
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rateTx 未在解除行锁后完成")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var count int
	var expires time.Time
	if err := db.pool.QueryRow(ctx, `SELECT count,expires_at FROM auth_rates WHERE key=$1`, key).Scan(&count, &expires); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("过期窗口未重置：count=%d", count)
	}
	if time.Until(expires) < 30*time.Second {
		t.Fatalf("新窗口没有从解除锁后的当前时间开始：expires_at=%s", expires)
	}
}
