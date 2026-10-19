package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWaitLockBoundsPoolWait(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct {
		name          string
		wait, timeout time.Duration
		want          error
	}{
		{"锁等待先到期", 50 * time.Millisecond, 2 * time.Second, ErrLockBusy},
		{"请求先到期", 2 * time.Second, 50 * time.Millisecond, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := db.pool.Config()
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			// 占住唯一连接，让共享锁必须等待连接池，而非等待另一把数据库锁。
			conn, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			a := &Service{store: &Store{pool: pool}}
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			defer cancel()
			release, err := a.WaitLock(ctx, t.Name(), tc.wait)
			if release != nil {
				release()
				t.Fatal("连接池耗尽时意外取得锁")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("锁等待错误=%v，期望=%v", err, tc.want)
			}
			if tc.want == ErrLockBusy && ctx.Err() != nil {
				t.Fatalf("锁等待期限未覆盖连接池等待：%v", ctx.Err())
			}
			conn.Release()
			// 等待失败不能泄漏池连接；恢复容量后仍能正常取得并释放锁。
			release, err = a.WaitLock(t.Context(), t.Name(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
				t.Fatalf("等待失败后泄漏连接：%d", acquired)
			}
		})
	}
}

func TestWaitLockDeadlineDoesNotReleaseHeldLock(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	name := "test:" + t.Name()
	release, err := a.WaitLock(t.Context(), name, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(release)
	defer release()
	// 第二次等待超过第一次的获取期限，持有的锁仍必须有效。
	if next, err := a.WaitLock(t.Context(), name, 750*time.Millisecond); next != nil || !errors.Is(err, ErrLockBusy) {
		if next != nil {
			next()
		}
		t.Fatalf("获取期限结束后已持有的锁失效：%v", err)
	}
	release()
	release, err = a.WaitLock(t.Context(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
