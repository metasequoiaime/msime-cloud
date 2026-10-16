package account

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestRateLeaseAcrossPools(t *testing.T) {
	db := testStore(t)
	other, err := Open(t.Context(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	first, second := &Service{store: db}, &Service{store: other}
	ctx := t.Context()
	const scope, subject = "synthetic-lease", "group/member"
	until, err := first.AcquireRateLease(ctx, scope, subject, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if _, err := second.AcquireRateLease(ctx, scope, subject, time.Minute); !errors.Is(err, ErrLimited) {
				t.Error("占用失败应只返回限流，不改变租约", err)
			}
		})
	}
	workers.Wait()
	var count int
	var stored time.Time
	if err := db.pool.QueryRow(ctx, `SELECT count,expires_at FROM auth_rates WHERE key=$1`, scope+":"+hash(subject)).Scan(&count, &stored); err != nil || count != 1 || !stored.Equal(until) {
		t.Fatal("竞争者修改了持有者", count, stored, err)
	}
	if db.pool.Stat().AcquiredConns() != 0 || other.pool.Stat().AcquiredConns() != 0 {
		t.Fatal("租约持有期间不应占用池连接")
	}
	if err := first.ReleaseRateLease(ctx, scope, subject, until); err != nil {
		t.Fatal(err)
	}
	next, err := second.AcquireRateLease(ctx, scope, subject, time.Minute)
	if err != nil || next.Equal(until) {
		t.Fatal("释放后未生成独立租约", next, err)
	}
	for range 2 {
		if err := first.ReleaseRateLease(ctx, scope, subject, until); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.AcquireRateLease(ctx, scope, subject, time.Minute); !errors.Is(err, ErrLimited) {
		t.Fatal("旧租约或重复释放影响了新持有者", err)
	}
	if err := second.ReleaseRateLease(ctx, scope, subject, next); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AcquireRateLease(ctx, scope, subject, time.Minute); err != nil {
		t.Fatal("当前持有者无法释放", err)
	}
	claimed := make(chan time.Time, 8)
	for range cap(claimed) {
		workers.Go(func() {
			lease, err := second.AcquireRateLease(ctx, scope, "unclaimed", time.Minute)
			if err == nil {
				claimed <- lease
			} else if !errors.Is(err, ErrLimited) {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if len(claimed) != 1 {
		t.Fatal("并发首次占用不唯一", len(claimed))
	}
}

func TestRateLeaseExpiredAndLegacyRows(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := t.Context()
	const scope, subject = "synthetic-lease", "legacy"
	for _, count := range []int{0, 2} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,$2,clock_timestamp()+interval '1 hour') ON CONFLICT(key) DO UPDATE SET count=$2,expires_at=clock_timestamp()-interval '1 second'`, scope+":"+hash(subject), count); err != nil {
			t.Fatal(err)
		}
		if _, err := a.AcquireRateLease(ctx, scope, subject, time.Minute); err != nil {
			t.Fatal("已释放或已过期的旧计数不能恢复占用", count, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.AcquireRateLease(canceled, scope, "canceled", time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal("已取消的请求占用了租约", err)
	}
	if err := a.ReleaseRateLease(canceled, scope, subject, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatal("取消释放未返回错误", err)
	}
	if _, err := a.AcquireRateLease(ctx, scope, subject, time.Minute); !errors.Is(err, ErrLimited) {
		t.Fatal("错误释放清除了有效租约", err)
	}
}

func TestRateLeaseWindowStartsAfterLockWait(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const scope, subject = "synthetic-lease", "locked"
	key := scope + ":" + hash(subject)
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_rates(key,count,expires_at) VALUES($1,1,clock_timestamp()-interval '1 hour')`, key); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT key FROM auth_rates WHERE key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	type result struct {
		until time.Time
		err   error
	}
	done := make(chan result, 1)
	go func() {
		until, err := a.AcquireRateLease(ctx, scope, subject, time.Minute)
		done <- result{until, err}
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	var unlockedAt time.Time
	if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&unlockedAt); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.until.Before(unlockedAt.Add(time.Minute)) {
		t.Fatal("等待锁缩短了新租约", got.until, got.err)
	}
}
