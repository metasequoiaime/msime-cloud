package account

import (
	"context"
	"testing"
	"time"
)

func TestQQDraftWritesRejectExpirationDuringLockWait(t *testing.T) {
	testQQDraftWritesAfterLockWait(t, true)
}

func TestQQDraftWritesAcceptClaimExpiredDuringLockWait(t *testing.T) {
	testQQDraftWritesAfterLockWait(t, false)
}

func testQQDraftWritesAfterLockWait(t *testing.T, expires bool) {
	t.Helper()
	for _, operation := range []string{"supplement", "claim", "dismiss"} {
		t.Run(operation, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			id, err := a.CreateQQDraft(ctx, "G/synthetic-member", "原话", `{"kind":"bug"}`)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := a.QQDraft(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM qq_drafts WHERE id=$1 FOR UPDATE`, id); err != nil {
				t.Fatal(err)
			}
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				var ok bool
				var err error
				switch operation {
				case "supplement":
					ok, err = a.UpdateQQDraft(ctx, previous, "补充后的原话", `{"kind":"feature"}`)
				case "claim":
					_, ok, err = a.ClaimQQDraftSnapshot(ctx, id)
				case "dismiss":
					ok, err = a.DismissQQDraft(ctx, id)
				}
				done <- result{ok, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 在写请求确实等锁后移动边界：分别验证草稿已过期，以及认领已超时可再次操作。
			query, window := `UPDATE qq_drafts SET created_at=clock_timestamp()-$2::interval WHERE id=$1`, QQDraftWindow
			if !expires {
				query, window = `UPDATE qq_drafts SET filed_at=clock_timestamp()-$2::interval WHERE id=$1`, QQDraftClaimTimeout
			}
			if _, err := blocker.Exec(ctx, query, id, window.String()); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || got.ok != !expires {
				t.Errorf("锁后的草稿可写状态错误：expires=%v ok=%v err=%v", expires, got.ok, got.err)
			}
			if !expires {
				return
			}
			var unchanged bool
			if err := db.pool.QueryRow(ctx, `SELECT original=$2 AND report=$3 AND filed_at IS NULL AND issue_ref='' FROM qq_drafts WHERE id=$1`, id, previous.Original, previous.Report).Scan(&unchanged); err != nil || !unchanged {
				t.Errorf("过期草稿的内容或认领状态被修改：unchanged=%v err=%v", unchanged, err)
			}
		})
	}
}
