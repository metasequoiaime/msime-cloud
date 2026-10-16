package account

import (
	"context"
	"testing"
	"time"
)

func TestRateWindowStartsAfterLockWait(t *testing.T) {
	for _, operation := range []string{"rate", "transaction", "retry_after"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			key := "synthetic-rate-window:" + randomToken()
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
			const window = time.Minute
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "rate":
					done <- db.Rate(ctx, key, 1, window)
				case "retry_after":
					_, err := db.RateUntil(ctx, key, 1, window)
					done <- err
				case "transaction":
					tx, err := db.pool.Begin(ctx)
					if err != nil {
						done <- err
						return
					}
					defer tx.Rollback(context.Background())
					if err := rateTx(ctx, tx, key, 1, window); err != nil {
						done <- err
						return
					}
					done <- tx.Commit(ctx)
				}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 记录解除锁前的数据库时钟，无需等待整段限流窗口即可验证新窗口没有被等锁时间缩短。
			var unlockedAt time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&unlockedAt); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal("新窗口记次失败", err)
			}
			var expires time.Time
			var count int
			if err := db.pool.QueryRow(ctx, `SELECT count,expires_at FROM auth_rates WHERE key=$1`, key).Scan(&count, &expires); err != nil {
				t.Fatal(err)
			}
			if count != 1 || expires.Before(unlockedAt.Add(window)) {
				t.Fatalf("新窗口没有从锁后开始：count=%d expires=%s earliest=%s", count, expires, unlockedAt.Add(window))
			}
		})
	}
}
