package account

import (
	"context"
	"testing"
	"time"
)

func TestQQFAQCandidateWritesAfterLockWait(t *testing.T) {
	for _, expires := range []bool{true, false} {
		for _, operation := range []string{"claim", "ignore"} {
			boundary := "expired_candidate"
			if !expires {
				boundary = "expired_claim"
			}
			t.Run(boundary+"/"+operation, func(t *testing.T) {
				db := testStore(t)
				a := &Service{store: db}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				id, err := a.AddQQFAQCandidate(ctx, "合成问题", "合成答案")
				if err != nil {
					t.Fatal(err)
				}
				blocker, err := db.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				if _, err := blocker.Exec(ctx, `SELECT id FROM qq_faq_candidates WHERE id=$1 FOR UPDATE`, id); err != nil {
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
					if operation == "claim" {
						_, ok, err = a.ClaimQQFAQCandidateLease(ctx, id)
					} else {
						ok, err = a.CloseQQFAQCandidate(ctx, id, "ignored", "")
					}
					done <- result{ok, err}
				}()
				waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
				// 请求等锁后移动时间边界，验证判断依据是取得锁后的时钟。
				query, window := `UPDATE qq_faq_candidates SET created_at=clock_timestamp()-$2::interval WHERE id=$1`, QQFAQCandidateWindow
				if !expires {
					query, window = `UPDATE qq_faq_candidates SET state='writing',claimed_at=clock_timestamp()-$2::interval WHERE id=$1`, QQFAQCandidateClaimTimeout
				}
				if _, err := blocker.Exec(ctx, query, id, window.String()); err != nil {
					t.Fatal(err)
				}
				if err := blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				got := <-done
				if got.err != nil || got.ok != !expires {
					t.Errorf("锁后的候选可写状态错误：expires=%v ok=%v err=%v", expires, got.ok, got.err)
				}
				if expires {
					var unchanged bool
					if err := db.pool.QueryRow(ctx, `SELECT state='open' AND claimed_at IS NULL AND ref='' FROM qq_faq_candidates WHERE id=$1`, id).Scan(&unchanged); err != nil || !unchanged {
						t.Errorf("过期候选状态被修改：unchanged=%v err=%v", unchanged, err)
					}
				}
			})
		}
	}
}

func TestQQFAQCandidateCompletionAfterExpiration(t *testing.T) {
	for _, lease := range []bool{false, true} {
		name := "legacy"
		if lease {
			name = "lease"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx := t.Context()
			id, err := a.AddQQFAQCandidate(ctx, "合成问题", "合成答案")
			if err != nil {
				t.Fatal(err)
			}
			claimedAt, ok, err := a.ClaimQQFAQCandidateLease(ctx, id)
			if err != nil || !ok {
				t.Fatal("认领失败", ok, err)
			}
			if _, err := db.pool.Exec(ctx, `UPDATE qq_faq_candidates SET created_at=clock_timestamp()-$2::interval WHERE id=$1`, id, QQFAQCandidateWindow.String()); err != nil {
				t.Fatal(err)
			}
			// PR 已经创建成功时，即使候选刚刚过期，也要保存完成记录。
			if lease {
				ok, err = a.CloseQQFAQCandidateLease(ctx, id, claimedAt, "done", "synthetic #1")
			} else {
				ok, err = a.CloseQQFAQCandidate(ctx, id, "done", "synthetic #1")
			}
			if err != nil || !ok {
				t.Fatal("过期阻止了完成收尾", ok, err)
			}
		})
	}
}
