package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAppleWebGrantUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_challenges ADD CONSTRAINT apple_grant_test_reject CHECK(verified_subject<>'synthetic-rejected') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE auth_challenges DROP CONSTRAINT IF EXISTS apple_grant_test_reject`); err != nil {
			t.Error(err)
		}
	})
	trace := &statementCancellation{}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	for _, tc := range []struct {
		name, provider             string
		expired, granted, rejected bool
	}{
		{name: "有效挑战", provider: "apple_web"},
		{name: "已过期", provider: "apple_web", expired: true},
		{name: "已签发", provider: "apple_web", granted: true},
		{name: "其他提供方", provider: "email"},
		{name: "不存在"},
		{name: "写入失败", provider: "apple_web", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			idHash, grantHash := hash(randomToken()), hash(randomToken())
			var before json.RawMessage
			if tc.provider != "" {
				expires := time.Now().Add(5 * time.Minute)
				if tc.expired {
					expires = time.Now().Add(-time.Minute)
				}
				var granted any
				if tc.granted {
					granted = time.Now()
				}
				if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,nonce,redirect_uri,code_challenge,expires_at,grant_hash,granted_at,verified_subject) VALUES($1,$2,'synthetic-nonce','app.msime.android','synthetic-pkce',$3,'previous-grant',$4,'previous-subject')`, idHash, tc.provider, expires, granted); err != nil {
					t.Fatal(err)
				}
				if err := db.pool.QueryRow(ctx, `SELECT to_jsonb(c) FROM auth_challenges c WHERE id_hash=$1`, idHash).Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			profile := appleWebChallenge{Subject: "synthetic-subject", Email: "apple@example.test", EmailVerified: true, Name: "合成用户"}
			if tc.rejected {
				profile.Subject = "synthetic-rejected"
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			err := store.grantAppleWeb(ctx, idHash, profile, grantHash)
			trace.mu.Lock()
			calls := trace.seen
			trace.mu.Unlock()
			if calls != 1 {
				t.Errorf("Apple 授权码签发用了 %d 次数据库调用，期望 1 次", calls)
			}
			valid := tc.provider == "apple_web" && !tc.expired && !tc.granted && !tc.rejected
			if tc.rejected {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23514" {
					t.Fatal("写入失败未返回约束错误", err)
				}
			} else if valid {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("无效挑战未被拒绝", err)
			}
			if !valid && tc.provider != "" {
				var after json.RawMessage
				if err := db.pool.QueryRow(ctx, `SELECT to_jsonb(c) FROM auth_challenges c WHERE id_hash=$1`, idHash).Scan(&after); err != nil || string(after) != string(before) {
					t.Fatal("签发失败改变了挑战行", err)
				}
			}
			if valid {
				var preserved bool
				if err := db.pool.QueryRow(ctx, `SELECT nonce='synthetic-nonce' AND redirect_uri='app.msime.android' AND code_challenge='synthetic-pkce' AND verified_subject=$2 AND verified_email=$3 AND verified_email_verified=$4 AND verified_name=$5 AND grant_hash=$6 AND granted_at IS NOT NULL AND expires_at>clock_timestamp() FROM auth_challenges WHERE id_hash=$1`, idHash, profile.Subject, profile.Email, profile.EmailVerified, profile.Name, grantHash).Scan(&preserved); err != nil || !preserved {
					t.Fatal("签发字段不完整或改变了挑战信息", preserved, err)
				}
			}
		})
	}
}

func TestAppleWebGrantConcurrentCallbacksKeepOneGrant(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	idHash := hash(randomToken())
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,expires_at) VALUES($1,'apple_web',clock_timestamp()+interval '5 minutes')`, idHash); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int
		err error
	}
	start := make(chan struct{})
	done := make(chan result, 8)
	for i := range 8 {
		go func() {
			<-start
			profile := appleWebChallenge{Subject: fmt.Sprintf("synthetic-subject-%d", i), Name: fmt.Sprintf("合成用户 %d", i)}
			done <- result{i, db.grantAppleWeb(ctx, idHash, profile, hash(fmt.Sprintf("synthetic-grant-%d", i)))}
		}()
	}
	close(start)
	winner, successes := -1, 0
	for range 8 {
		got := <-done
		if got.err == nil {
			winner, successes = got.n, successes+1
		} else if !errors.Is(got.err, ErrInvalid) {
			t.Error(got.err)
		}
	}
	if successes != 1 {
		t.Fatal("同一挑战签发成功次数不为 1", successes)
	}
	var subject, name, grant string
	if err := db.pool.QueryRow(ctx, `SELECT verified_subject,verified_name,grant_hash FROM auth_challenges WHERE id_hash=$1`, idHash).Scan(&subject, &name, &grant); err != nil || subject != fmt.Sprintf("synthetic-subject-%d", winner) || name != fmt.Sprintf("合成用户 %d", winner) || grant != hash(fmt.Sprintf("synthetic-grant-%d", winner)) {
		t.Fatal("并发回调覆盖了已签发的身份或授权码", subject, name, err)
	}
}

func TestAppleWebGrantUsesStateCommittedDuringLockWait(t *testing.T) {
	for _, granted := range []bool{true, false} {
		name := "等待期间已签发"
		if !granted {
			name = "等待期间延长有效期"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			idHash, requested := hash(randomToken()), hash(randomToken())
			expires := time.Now().Add(5 * time.Minute)
			if !granted {
				expires = time.Now().Add(-time.Minute)
			}
			if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,expires_at) VALUES($1,'apple_web',$2)`, idHash, expires); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id_hash FROM auth_challenges WHERE id_hash=$1 FOR UPDATE`, idHash); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- db.grantAppleWeb(ctx, idHash, appleWebChallenge{Subject: "requested-subject"}, requested)
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if granted {
				_, err = blocker.Exec(ctx, `UPDATE auth_challenges SET verified_subject='first-subject',grant_hash='first-grant',granted_at=clock_timestamp() WHERE id_hash=$1`, idHash)
			} else {
				_, err = blocker.Exec(ctx, `UPDATE auth_challenges SET expires_at=clock_timestamp()+interval '5 minutes' WHERE id_hash=$1`, idHash)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			if granted && !errors.Is(err, ErrInvalid) || !granted && err != nil {
				t.Fatal("未按取得锁后的挑战状态处理", err)
			}
			wantSubject, wantGrant := "requested-subject", requested
			if granted {
				wantSubject, wantGrant = "first-subject", "first-grant"
			}
			var subject, grant string
			if err := db.pool.QueryRow(ctx, `SELECT verified_subject,grant_hash FROM auth_challenges WHERE id_hash=$1`, idHash).Scan(&subject, &grant); err != nil || subject != wantSubject || grant != wantGrant {
				t.Fatal("等待期间提交的状态被错误覆盖", subject, err)
			}
		})
	}
}
