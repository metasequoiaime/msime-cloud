package account

import (
	"errors"
	"testing"
	"time"
)

func TestRateUntilUsesWallClockAfterAuthRateLockWait(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	key := "rate-until-clock-" + randomToken()
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
	result := make(chan struct {
		wait time.Duration
		err  error
	}, 1)
	go func() {
		wait, err := db.RateUntil(ctx, key, 1, time.Minute)
		result <- struct {
			wait time.Duration
			err  error
		}{wait, err}
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	time.Sleep(700 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if errors.Is(got.err, ErrLimited) {
			t.Fatal("限流窗口已过期却仍被拒绝")
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.wait != 0 {
			t.Fatalf("新窗口不应返回剩余等待时间：%s", got.wait)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RateUntil 未在解除行锁后完成")
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

func TestReleaseRateDoesNotRefundExpiredWindowAfterAuthRateLockWait(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	key := "release-rate-clock-" + randomToken()
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,2,clock_timestamp()+interval '500 milliseconds')`, key); err != nil {
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
	done := make(chan error, 1)
	go func() { done <- db.ReleaseRate(ctx, key) }()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	time.Sleep(700 * time.Millisecond)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReleaseRate 未在解除行锁后完成")
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("过期窗口不应被退回：count=%d", count)
	}
}
