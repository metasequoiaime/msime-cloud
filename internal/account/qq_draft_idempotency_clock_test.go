package account

import (
	"context"
	"testing"
	"time"
)

func TestQQDraftCreateOnceUsesClockAfterLockWait(t *testing.T) {
	for _, kind := range []string{"new", "expired"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			subject, original, report := "G/"+randomToken(), "合成反馈", `{"kind":"bug"}`
			var previous int64
			if kind == "expired" {
				var err error
				previous, err = a.CreateQQDraft(ctx, subject, original, report)
				if err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || chr(31) || $2 || chr(31) || $3))`, QQDraftSubject(subject), original, report); err != nil {
				t.Fatal(err)
			}
			type result struct {
				id  int64
				err error
			}
			done := make(chan result, 1)
			go func() {
				id, err := a.CreateQQDraftOnce(ctx, subject, original, report)
				done <- result{id, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if previous != 0 {
				// 确认请求已等锁后把旧草稿移到过期边界；事务开始时钟仍会认为它可以复用。
				if _, err := blocker.Exec(ctx, `UPDATE qq_drafts SET created_at=clock_timestamp()-$2::interval WHERE id=$1`, previous, QQDraftIdempotencyWindow.String()); err != nil {
					t.Fatal(err)
				}
			}
			var unlockedAt time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&unlockedAt); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || got.id <= 0 {
				t.Fatal("创建草稿失败", got.id, got.err)
			}
			if got.id == previous {
				t.Fatal("复用了等待锁期间已经超过幂等窗口的草稿", got.id)
			}
			draft, err := a.QQDraft(ctx, got.id)
			if err != nil {
				t.Fatal(err)
			}
			if draft.CreatedAt.Before(unlockedAt) || draft.UpdatedAt.Before(unlockedAt) {
				t.Errorf("新草稿在锁释放前就已计时：created_at=%s updated_at=%s earliest=%s", draft.CreatedAt, draft.UpdatedAt, unlockedAt)
			}
			if id, err := a.CreateQQDraftOnce(ctx, subject, original, report); err != nil || id != got.id {
				t.Errorf("幂等窗口内未复用新草稿：id=%d want=%d err=%v", id, got.id, err)
			}
		})
	}
}
