package account

import (
	"context"
	"testing"
	"time"
)

func TestCreateSkinJobInitialHeartbeatUsesTimeAfterLockWait(t *testing.T) {
	for _, lock := range []string{"客户端全局锁", "用户全局锁", "用户账号锁"} {
		t.Run(lock, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
				t.Fatal(err)
			}
			owner := "synthetic-client"
			var uid string
			if lock != "客户端全局锁" {
				uid = complete(t, db, Identity{"email", "skin-clock@example.test"}).User.ID
				owner = "user:" + uid
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if lock == "用户账号锁" {
				_, err = blocker.Exec(ctx, `SELECT id FROM auth_users WHERE id=$1 FOR UPDATE`, uid)
			} else {
				_, err = blocker.Exec(ctx, skinJobsLock)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			a := &Service{store: db}
			id := skinJobID('a')
			go func() { _, _, err := a.CreateSkinJob(ctx, id, owner, 1, 1, 10, time.Minute); done <- err }()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 只在创建事务确实等待后记录数据库时间，心跳必须从后续插入开始计算。
			var releasedAfter time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAfter); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var heartbeat, expires time.Time
			if err := db.pool.QueryRow(ctx, `SELECT heartbeat_at,expires_at FROM skin_jobs WHERE id=$1`, id).Scan(&heartbeat, &expires); err != nil {
				t.Fatal(err)
			}
			if heartbeat.Before(releasedAfter) {
				t.Errorf("新任务心跳沿用了锁等待前的时间：heartbeat=%s，释放锁前=%s", heartbeat, releasedAfter)
			}
			if expires.Before(releasedAfter.Add(time.Minute)) {
				t.Error("新任务有效期包含了锁等待时间")
			}
			if job, err := a.GetSkinJob(ctx, id, owner); err != nil || job.State != "running" {
				t.Fatal("新任务不可运行", job, err)
			}
			if alive, err := a.SkinJobHeartbeat(ctx, id); err != nil || !alive {
				t.Fatal("新任务无法刷新心跳", alive, err)
			}
			if recorded, err := a.FinishSkinJob(ctx, id, "succeeded", "", []byte(`{}`)); err != nil || !recorded {
				t.Fatal("新任务无法保存结果", recorded, err)
			}
		})
	}
}
