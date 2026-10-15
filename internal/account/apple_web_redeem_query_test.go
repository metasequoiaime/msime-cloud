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

func TestAppleWebRedeemUsesOneDatabaseCall(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_challenges ADD CONSTRAINT apple_redeem_test_reject CHECK(verified_subject<>'synthetic-rejected' OR grant_hash<>'') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE auth_challenges DROP CONSTRAINT IF EXISTS apple_redeem_test_reject`); err != nil {
			t.Error(err)
		}
	})
	user := randomToken()
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_users(id) VALUES($1)`, user); err != nil {
		t.Fatal(err)
	}
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
		name, provider                   string
		linked, expired, stale, rejected bool
	}{
		{name: "有效授权码", provider: "apple_web"},
		{name: "绑定授权码", provider: "apple_web", linked: true},
		{name: "挑战已过期", provider: "apple_web", expired: true},
		{name: "签发时间已过期", provider: "apple_web", stale: true},
		{name: "其他提供方", provider: "email"},
		{name: "不存在"},
		{name: "写入失败", provider: "apple_web", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			idHash, grantHash := hash(randomToken()), hash(randomToken())
			want := appleWebChallenge{IDHash: idHash, App: "app.msime.android", CodeChallenge: "synthetic-pkce", Subject: "synthetic-subject", Email: "apple@example.test", EmailVerified: true, Name: "合成用户"}
			if tc.linked {
				want.LinkUser = user
			}
			if tc.rejected {
				want.Subject = "synthetic-rejected"
			}
			var before json.RawMessage
			if tc.provider != "" {
				expires, granted := time.Now().Add(time.Minute), time.Now()
				if tc.expired {
					expires = time.Now().Add(-time.Minute)
				}
				if tc.stale {
					granted = time.Now().Add(-5 * time.Minute)
				}
				if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,nonce,link_user,redirect_uri,code_challenge,expires_at,grant_hash,granted_at,verified_subject,verified_email,verified_email_verified,verified_name) VALUES($1,$2,'synthetic-nonce',NULLIF($3,''),$4,$5,$6,$7,$8,$9,$10,$11,$12)`, idHash, tc.provider, want.LinkUser, want.App, want.CodeChallenge, expires, grantHash, granted, want.Subject, want.Email, want.EmailVerified, want.Name); err != nil {
					t.Fatal(err)
				}
				if err := db.pool.QueryRow(ctx, `SELECT to_jsonb(c)-'grant_hash' FROM auth_challenges c WHERE id_hash=$1`, idHash).Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			got, err := store.redeemAppleWeb(ctx, grantHash)
			trace.mu.Lock()
			calls := trace.seen
			trace.mu.Unlock()
			if calls != 1 {
				t.Errorf("Apple 授权码兑换用了 %d 次数据库调用，期望 1 次", calls)
			}
			valid := tc.provider == "apple_web" && !tc.expired && !tc.stale && !tc.rejected
			if tc.rejected {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23514" {
					t.Fatal("消费失败未返回约束错误", err)
				}
			} else if valid {
				if err != nil || got != want {
					t.Fatal("兑换未返回完整挑战资料", got, err)
				}
			} else if !errors.Is(err, ErrInvalid) {
				t.Fatal("无效授权码未被拒绝", err)
			}
			if tc.provider != "" {
				var after json.RawMessage
				var remaining string
				if err := db.pool.QueryRow(ctx, `SELECT to_jsonb(c)-'grant_hash',grant_hash FROM auth_challenges c WHERE id_hash=$1`, idHash).Scan(&after, &remaining); err != nil || string(after) != string(before) {
					t.Fatal("消费授权码改变了其他挑战字段", err)
				}
				if valid && remaining != "" || !valid && remaining != grantHash {
					t.Fatal("授权码消费状态不正确", remaining)
				}
			}
		})
	}
}

func TestAppleWebRedeemDuplicateHashConsumesOnlyReturnedChallenge(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	grantHash := hash(randomToken())
	for range 2 {
		idHash := hash(randomToken())
		if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,grant_hash,granted_at,expires_at,verified_subject) VALUES($1,'apple_web',$2,clock_timestamp(),clock_timestamp()+interval '1 minute',$1)`, idHash, grantHash); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for remaining := 1; remaining >= 0; remaining-- {
		got, err := db.redeemAppleWeb(ctx, grantHash)
		if err != nil || got.IDHash == "" || got.Subject != got.IDHash || seen[got.IDHash] {
			t.Fatal("未返回本次消费的挑战", got, err)
		}
		seen[got.IDHash] = true
		var count int
		if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM auth_challenges WHERE grant_hash=$1`, grantHash).Scan(&count); err != nil || count != remaining {
			t.Fatal("一次兑换消费了多个挑战", count, err)
		}
	}
	if _, err := db.redeemAppleWeb(ctx, grantHash); !errors.Is(err, ErrInvalid) {
		t.Fatal("已消费的授权码仍可重放", err)
	}
}

func TestAppleWebRedeemConcurrentRequestsConsumeOnce(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	idHash, grantHash := hash(randomToken()), hash(randomToken())
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,grant_hash,granted_at,expires_at,verified_subject) VALUES($1,'apple_web',$2,clock_timestamp(),clock_timestamp()+interval '1 minute','synthetic-subject')`, idHash, grantHash); err != nil {
		t.Fatal(err)
	}
	start, done := make(chan struct{}), make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			got, err := db.redeemAppleWeb(ctx, grantHash)
			if err == nil && (got.IDHash != idHash || got.Subject != "synthetic-subject") {
				err = fmt.Errorf("兑换返回了其他挑战资料")
			}
			done <- err
		}()
	}
	close(start)
	successes := 0
	for range 8 {
		if err := <-done; err == nil {
			successes++
		} else if !errors.Is(err, ErrInvalid) {
			t.Error(err)
		}
	}
	if successes != 1 {
		t.Fatal("同一授权码兑换成功次数不为 1", successes)
	}
}

func TestAppleWebRedeemUsesStateCommittedDuringLockWait(t *testing.T) {
	for _, consumed := range []bool{true, false} {
		name := "等待期间已消费"
		if !consumed {
			name = "等待期间更新有效期和资料"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			idHash, grantHash := hash(randomToken()), hash(randomToken())
			if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,grant_hash,granted_at,expires_at,verified_subject) VALUES($1,'apple_web',$2,clock_timestamp()-interval '5 minutes',clock_timestamp()-interval '1 minute','previous-subject')`, idHash, grantHash); err != nil {
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
			type result struct {
				c   appleWebChallenge
				err error
			}
			done := make(chan result, 1)
			go func() {
				c, err := db.redeemAppleWeb(ctx, grantHash)
				done <- result{c, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if consumed {
				_, err = blocker.Exec(ctx, `UPDATE auth_challenges SET grant_hash='' WHERE id_hash=$1`, idHash)
			} else {
				_, err = blocker.Exec(ctx, `UPDATE auth_challenges SET granted_at=clock_timestamp(),expires_at=clock_timestamp()+interval '1 minute',verified_subject='current-subject',verified_email='current@example.test',verified_email_verified=true,verified_name='当前用户',redirect_uri='app.msime.android',code_challenge='current-pkce' WHERE id_hash=$1`, idHash)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if consumed {
				if !errors.Is(got.err, ErrInvalid) {
					t.Fatal("等待期间已消费的授权码仍可兑换", got.err)
				}
			} else {
				want := appleWebChallenge{IDHash: idHash, Subject: "current-subject", Email: "current@example.test", EmailVerified: true, Name: "当前用户", App: "app.msime.android", CodeChallenge: "current-pkce"}
				if got.err != nil || got.c != want {
					t.Fatal("未按取得锁后的有效期和资料兑换", got.c, got.err)
				}
			}
		})
	}
}

func TestAppleWebRedeemRejectsGrantAgeExpiredDuringLockWait(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	idHash, grantHash := hash(randomToken()), hash(randomToken())
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_challenges(id_hash,provider,grant_hash,granted_at,expires_at) VALUES($1,'apple_web',$2,clock_timestamp()-$3::interval+interval '1 second',clock_timestamp()+interval '5 minutes')`, idHash, grantHash, appleWebGrantTTL.String()); err != nil {
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
		_, err := db.redeemAppleWeb(ctx, grantHash)
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var expired bool
		if err := db.pool.QueryRow(ctx, `SELECT granted_at<=clock_timestamp()-$2::interval FROM auth_challenges WHERE id_hash=$1`, idHash, appleWebGrantTTL.String()).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("授权码签发时间未按时过期", ctx.Err())
		case <-ticker.C:
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalid) {
		t.Fatal("等待期间超出最长签发时间的授权码仍可兑换", err)
	}
	var remaining string
	if err := db.pool.QueryRow(ctx, `SELECT grant_hash FROM auth_challenges WHERE id_hash=$1`, idHash).Scan(&remaining); err != nil || remaining != grantHash {
		t.Fatal("拒绝兑换时清空了授权码", err)
	}
}
