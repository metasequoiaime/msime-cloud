package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdminSessionRechecksAfterActivityLockWait(t *testing.T) {
	for _, state := range []string{"过期", "撤销", "缩短有效期", "仍有效", "撤销回滚", "并发刷新"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			want := AdminIdentity{Subject: "session-recheck", Email: "session-recheck@example.test", Name: "测试管理员", UserAgent: "synthetic-agent"}
			token, err := a.CreateAdminSession(ctx, want)
			if err != nil {
				t.Fatal(err)
			}
			var before time.Time
			if err := db.pool.QueryRow(ctx, `UPDATE admin_sessions SET last_seen_at=clock_timestamp()-interval '10 minutes',
 expires_at=CASE WHEN $2 THEN clock_timestamp()+interval '2 seconds' ELSE expires_at END
 WHERE token_hash=$1 RETURNING last_seen_at`, hash(token), state == "过期").Scan(&before); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM admin_sessions WHERE token_hash=$1 FOR UPDATE`, hash(token)); err != nil {
				t.Fatal(err)
			}
			type result struct {
				identity AdminIdentity
				err      error
			}
			done := make(chan result, 1)
			go func() {
				identity, err := a.AdminSession(ctx, token)
				done <- result{identity, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			var refreshedAt time.Time
			switch state {
			case "过期":
				for {
					var expired bool
					if err := db.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM admin_sessions WHERE token_hash=$1`, hash(token)).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("会话未在期限内过期")
					case <-time.After(10 * time.Millisecond):
					}
				}
			case "撤销", "撤销回滚":
				if _, err := blocker.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash=$1`, hash(token)); err != nil {
					t.Fatal(err)
				}
			case "缩短有效期":
				if _, err := blocker.Exec(ctx, `UPDATE admin_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE token_hash=$1`, hash(token)); err != nil {
					t.Fatal(err)
				}
			case "并发刷新":
				if err := blocker.QueryRow(ctx, `UPDATE admin_sessions SET last_seen_at=clock_timestamp() WHERE token_hash=$1 RETURNING last_seen_at`, hash(token)).Scan(&refreshedAt); err != nil {
					t.Fatal(err)
				}
			}
			var releasedAt time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAt); err != nil {
				t.Fatal(err)
			}
			if state == "撤销回滚" {
				err = blocker.Rollback(ctx)
			} else {
				err = blocker.Commit(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := <-done
			valid := state == "仍有效" || state == "撤销回滚" || state == "并发刷新"
			if valid {
				if got.err != nil || got.identity != want {
					t.Fatal("有效会话未返回原身份", got.identity, got.err)
				}
			} else if !errors.Is(got.err, ErrInvalid) || got.identity != (AdminIdentity{}) {
				t.Errorf("锁等待后失效的会话仍被接受：身份=%+v，错误=%v", got.identity, got.err)
			}
			if state != "撤销" {
				var seen time.Time
				if err := db.pool.QueryRow(ctx, `SELECT last_seen_at FROM admin_sessions WHERE token_hash=$1`, hash(token)).Scan(&seen); err != nil {
					t.Fatal(err)
				}
				if state == "并发刷新" && !seen.Equal(refreshedAt) {
					t.Error("等待期间已刷新的活动时间被重复改写")
				} else if state != "并发刷新" && valid && seen.Before(releasedAt) {
					t.Error("最近活动时间仍使用锁等待前的时钟")
				} else if !valid && !seen.Equal(before) {
					t.Error("失效会话的最近活动时间被刷新")
				}
			}
		})
	}
}

func TestAdminSessionKeepsRecentActivityTime(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	want := AdminIdentity{Subject: "recent-session", Email: "recent-session@example.test"}
	token, err := a.CreateAdminSession(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	if err := db.pool.QueryRow(ctx, `SELECT last_seen_at FROM admin_sessions WHERE token_hash=$1`, hash(token)).Scan(&before); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM admin_sessions WHERE token_hash=$1 FOR UPDATE`, hash(token)); err != nil {
		t.Fatal(err)
	}
	got, err := a.AdminSession(ctx, token)
	if err != nil || got != want {
		t.Fatal("近期活跃会话未返回原身份", got, err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT last_seen_at FROM admin_sessions WHERE token_hash=$1`, hash(token)).Scan(&after); err != nil || !after.Equal(before) {
		t.Fatal("近期活跃会话被重复写入", err)
	}
}
