package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQQLinkCodeRechecksAfterLockWait(t *testing.T) {
	for _, state := range []string{"用户锁期间过期", "绑定码锁期间过期", "缩短有效期", "删除", "仍有效", "删除回滚"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			user := complete(t, db, Identity{"email", randomToken()[:16] + "@example.test"})
			subject := "G/" + randomToken()
			code, err := (&Service{store: db}).CreateQQLinkCode(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			expires := state == "用户锁期间过期" || state == "绑定码锁期间过期"
			if expires {
				if _, err := db.pool.Exec(ctx, `UPDATE qq_link_codes SET expires_at=clock_timestamp()+interval '2 seconds' WHERE code_hash=$1`, hash(code)); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if state == "用户锁期间过期" {
				_, err = blocker.Exec(ctx, `SELECT id FROM auth_users WHERE id=$1 FOR UPDATE`, user.User.ID)
			} else {
				_, err = blocker.Exec(ctx, `SELECT code_hash FROM qq_link_codes WHERE code_hash=$1 FOR UPDATE`, hash(code))
			}
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				link QQLink
				err  error
			}
			done := make(chan result, 1)
			go func() {
				link, err := db.RedeemQQLinkCode(ctx, user.User.ID, code)
				done <- result{link, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if expires {
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var expired bool
					if err := db.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM qq_link_codes WHERE code_hash=$1`, hash(code)).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("绑定码未在期限内过期")
					case <-ticker.C:
					}
				}
			} else if state == "缩短有效期" {
				_, err = blocker.Exec(ctx, `UPDATE qq_link_codes SET expires_at=clock_timestamp()-interval '1 second' WHERE code_hash=$1`, hash(code))
			} else if state == "删除" || state == "删除回滚" {
				_, err = blocker.Exec(ctx, `DELETE FROM qq_link_codes WHERE code_hash=$1`, hash(code))
			}
			if err != nil {
				t.Fatal(err)
			}
			if state == "删除回滚" {
				err = blocker.Rollback(ctx)
			} else {
				err = blocker.Commit(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := <-done
			valid := state == "仍有效" || state == "删除回滚"
			if valid {
				if got.err != nil || got.link.ID == "" {
					t.Fatal("有效绑定码未能兑换", got.link, got.err)
				}
				if _, err := db.RedeemQQLinkCode(ctx, user.User.ID, code); !errors.Is(err, ErrQQLinkCodeNotFound) {
					t.Fatal("绑定码被重复使用", err)
				}
			} else if !errors.Is(got.err, ErrQQLinkCodeNotFound) || got.link != (QQLink{}) {
				t.Errorf("锁等待后失效的绑定码仍被接受：绑定=%+v，错误=%v", got.link, got.err)
			}
			var linked bool
			if err := db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM qq_account_links WHERE subject=$1 AND user_id=$2)`, hash(subject), user.User.ID).Scan(&linked); err != nil || linked != valid {
				t.Error("绑定结果不符合锁后的有效状态", linked, err)
			}
			var codes int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM qq_link_codes WHERE code_hash=$1`, hash(code)).Scan(&codes); err != nil {
				t.Fatal(err)
			}
			if expires || state == "缩短有效期" {
				if codes != 1 {
					t.Error("兑换失败没有回滚绑定码删除")
				}
			} else if codes != 0 {
				t.Error("绑定码没有按预期被消费或删除")
			}
		})
	}
}

func TestQQLinkCodeTTLStartsAfterLockWait(t *testing.T) {
	for _, lock := range []string{"群友锁", "旧绑定码锁"} {
		t.Run(lock, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			a := &Service{store: db}
			subject := "G/" + randomToken()
			oldCode, err := a.CreateQQLinkCode(ctx, subject)
			if err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if lock == "群友锁" {
				_, err = blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "qq-link-code:"+hash(subject))
			} else {
				_, err = blocker.Exec(ctx, `SELECT code_hash FROM qq_link_codes WHERE code_hash=$1 FOR UPDATE`, hash(oldCode))
			}
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				code string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				code, err := a.CreateQQLinkCode(ctx, subject)
				done <- result{code, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			var releasedAt time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAt); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || got.code == "" {
				t.Fatal(got.err)
			}
			var expiry time.Time
			if err := db.pool.QueryRow(ctx, `SELECT expires_at FROM qq_link_codes WHERE code_hash=$1`, hash(got.code)).Scan(&expiry); err != nil {
				t.Fatal(err)
			}
			if expiry.Before(releasedAt.Add(QQLinkCodeTTL)) {
				t.Error("绑定码有效期从锁等待前开始计算")
			}
			var codes int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM qq_link_codes WHERE subject=$1`, hash(subject)).Scan(&codes); err != nil || codes != 1 {
				t.Fatal("重新生成后没有只保留一个绑定码", codes, err)
			}
		})
	}
}
