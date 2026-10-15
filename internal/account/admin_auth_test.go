package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdminFlowRejectsExpiryWhileWaitingForLock(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	state := randomToken()
	if err := a.SaveAdminFlow(ctx, state, AdminLoginFlow{Nonce: "nonce", Verifier: "verifier"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE admin_login_flows SET expires_at=clock_timestamp()+interval '1 second' WHERE state_hash=$1`, hash(state)); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT state_hash FROM admin_login_flows WHERE state_hash=$1 FOR UPDATE`, hash(state)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.ConsumeAdminFlow(ctx, state)
		done <- err
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	for {
		var expired bool
		if err := db.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM admin_login_flows WHERE state_hash=$1`, hash(state)).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("登录流程未在期限内过期")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalid) {
		t.Fatalf("等待行锁期间过期的登录流程仍被接受，错误 = %v", err)
	}
}

func TestAdminFlowConcurrentConsumptionOnlyOnce(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	state := randomToken()
	want := AdminLoginFlow{Nonce: "nonce", Verifier: "verifier"}
	if err := a.SaveAdminFlow(t.Context(), state, want); err != nil {
		t.Fatal(err)
	}
	type result struct {
		flow AdminLoginFlow
		err  error
	}
	start := make(chan struct{})
	done := make(chan result, 8)
	for range cap(done) {
		go func() {
			<-start
			flow, err := a.ConsumeAdminFlow(t.Context(), state)
			done <- result{flow, err}
		}()
	}
	close(start)
	accepted := 0
	for range cap(done) {
		got := <-done
		if got.err == nil {
			accepted++
			if got.flow != want {
				t.Fatalf("返回的登录流程 = %+v，期望 %+v", got.flow, want)
			}
		} else if !errors.Is(got.err, ErrInvalid) {
			t.Fatal(got.err)
		}
	}
	if accepted != 1 {
		t.Fatalf("并发消费成功次数 = %d，期望 1", accepted)
	}
}

func TestAdminFlowsAndSessions(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	state := randomToken()
	flow := AdminLoginFlow{Nonce: "nonce", Verifier: "verifier"}
	if err := a.SaveAdminFlow(ctx, state, flow); err != nil {
		t.Fatal(err)
	}
	result, err := a.ConsumeAdminFlow(ctx, state)
	if err != nil || result != flow {
		t.Fatal(result, err)
	}
	if _, err = a.ConsumeAdminFlow(ctx, state); err != ErrInvalid {
		t.Fatal("replay accepted", err)
	}
	if err = a.SaveAdminFlow(ctx, state, flow); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE admin_login_flows SET expires_at=now()-interval '1 second' WHERE state_hash=$1`, hash(state)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ConsumeAdminFlow(ctx, state); err != ErrInvalid {
		t.Fatal("expired state accepted")
	}
	identity := AdminIdentity{Subject: "google-123", Email: "admin@example.test"}
	token, err := a.CreateAdminSession(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = db.pool.QueryRow(ctx, `SELECT token_hash FROM admin_sessions WHERE token_hash=$1`, hash(token)).Scan(&stored); err != nil || stored == token {
		t.Fatal("raw token stored", err)
	}
	who, err := a.AdminSession(ctx, token)
	if err != nil || who != identity {
		t.Fatal(who, err)
	}
	if _, err = db.Authenticate(ctx, token); err != ErrInvalid {
		t.Fatal("admin session accepted as user")
	}
	if err = a.DeleteAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err = a.AdminSession(ctx, token); err != ErrInvalid {
		t.Fatal("logout failed")
	}
	token, err = a.CreateAdminSession(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, hash(token)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.AdminSession(ctx, token); err != ErrInvalid {
		t.Fatal("expired session accepted")
	}
}
