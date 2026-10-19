package account

import (
	"context"
	"testing"
	"time"
)

func TestQQBadAnswerFeedbackRechecksAfterLockWait(t *testing.T) {
	for _, state := range []string{"open", "done", "ignored", "expired"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if _, err := db.pool.Exec(ctx, `TRUNCATE qq_bad_answers`); err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := db.pool.QueryRow(ctx, `INSERT INTO qq_bad_answers(question,answer,feedback) VALUES('如何切换五笔','原回答','不对') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var createdAt time.Time
			if err := blocker.QueryRow(ctx, `SELECT created_at FROM qq_bad_answers WHERE id=$1 FOR UPDATE`, id).Scan(&createdAt); err != nil {
				t.Fatal(err)
			}
			within := 30 * time.Minute
			if state == "expired" {
				within = 5 * time.Second
			}
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := a.UpdateQQBadAnswerFeedback(ctx, "如何切换五笔", "还是不对", within)
				done <- result{ok, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if state == "open" {
				if _, err := blocker.Exec(ctx, `UPDATE qq_bad_answers SET feedback=feedback||'；并发反馈' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if state == "done" || state == "ignored" {
				if _, err := blocker.Exec(ctx, `UPDATE qq_bad_answers SET state=$2 WHERE id=$1`, id, state); err != nil {
					t.Fatal(err)
				}
			}
			if state == "expired" {
				// 只持有行锁，不修改记录；按数据库时钟确认等待已跨过合并窗口。
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var expired bool
					if err := db.pool.QueryRow(ctx, `SELECT clock_timestamp()>=$1::timestamptz+$2::interval`, createdAt, within.String()).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.err != nil || got.ok != (state == "open") {
					t.Errorf("锁后合并结果错误：ok=%v err=%v", got.ok, got.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var feedback, currentState string
			if err := db.pool.QueryRow(ctx, `SELECT feedback,state FROM qq_bad_answers WHERE id=$1`, id).Scan(&feedback, &currentState); err != nil {
				t.Fatal(err)
			}
			wantFeedback, wantState := "不对", state
			if state == "open" {
				wantFeedback += "；并发反馈；还是不对"
			}
			if state == "expired" {
				wantState = "open"
			}
			if feedback != wantFeedback || currentState != wantState {
				t.Errorf("差评内容或状态错误：feedback=%q state=%q", feedback, currentState)
			}
		})
	}
}
