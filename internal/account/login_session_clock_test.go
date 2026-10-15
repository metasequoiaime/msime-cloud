package account

import (
	"context"
	"testing"
	"time"
)

func TestLoginSessionLifetimeStartsAfterAccountLockWait(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	owner := complete(t, db, Identity{"email", "login-clock-owner@example.test"})
	challenge := Challenge{IDHash: hash(randomToken()), Provider: "google", Subject: "login-clock-linked@example.test", LinkUser: owner.User.ID}
	if err := db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM auth_users WHERE id=$1 FOR UPDATE`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := db.Complete(ctx, challenge, Identity{Provider: challenge.Provider, Subject: challenge.Subject})
		result <- err
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	// 解锁前读取数据库实时时钟；新会话的计时不能早于这个确定的时间点。
	var releasedAt time.Time
	if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var createdAt, accessExpires, expiresAt time.Time
	var refreshLifetime bool
	if err := db.pool.QueryRow(ctx, `SELECT created_at,access_expires,expires_at,expires_at=created_at+interval '30 days' FROM auth_sessions WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1`, owner.User.ID).Scan(&createdAt, &accessExpires, &expiresAt, &refreshLifetime); err != nil {
		t.Fatal(err)
	}
	if createdAt.Before(releasedAt) {
		t.Fatalf("登录会话在账号锁释放前就已计时：created_at=%s，锁释放=%s", createdAt, releasedAt)
	}
	if lifetime := accessExpires.Sub(createdAt); lifetime != 15*time.Minute {
		t.Fatalf("登录会话有效期不是锁后 15 分钟：created_at=%s，access_expires=%s，有效期=%s", createdAt, accessExpires, lifetime)
	}
	if !refreshLifetime {
		t.Fatalf("登录会话刷新有效期不是锁后 30 天：created_at=%s，expires_at=%s", createdAt, expiresAt)
	}
}
