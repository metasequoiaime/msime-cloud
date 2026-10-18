package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateAdminSessionChecksMembershipAndInsertsInOneQuery(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_sessions ADD CONSTRAINT admin_session_create_test_reject CHECK(subject<>'synthetic-rejected') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_sessions DROP CONSTRAINT IF EXISTS admin_session_create_test_reject`); err != nil {
			t.Error(err)
		}
	})
	cfg := db.pool.Config()
	trace := &statementCancellation{}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		name                              string
		member, disabled, rejected, owner bool
	}{
		{name: "没有成员记录的账号"},
		{name: "已启用成员", member: true},
		{name: "已禁用成员", member: true, disabled: true},
		{name: "没有成员记录的所有者", owner: true},
		{name: "保留启用成员记录的所有者", member: true, owner: true},
		{name: "保留禁用成员记录的所有者", member: true, disabled: true, owner: true},
		{name: "写入失败", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			email := "session-create-" + randomToken()[:8] + "@example.test"
			a.ConfigureAdmin(AdminSettings{})
			if tc.owner {
				a.ConfigureAdmin(AdminSettings{Owners: []string{email}})
			}
			if tc.member {
				if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email,role,enabled) VALUES($1,'reviewer',$2)`, email, !tc.disabled); err != nil {
					t.Fatal(err)
				}
			}
			identity := AdminIdentity{Subject: "synthetic-subject", Email: strings.ToUpper(email), Name: "管理员\x00", UserAgent: "synthetic-agent\n"}
			if tc.rejected {
				identity.Subject = "synthetic-rejected"
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			token, err := a.CreateAdminSession(ctx, identity)
			trace.mu.Lock()
			calls := trace.seen
			trace.mu.Unlock()
			if calls != 1 {
				t.Errorf("管理员会话创建用了 %d 次数据库调用，期望 1 次", calls)
			}
			if tc.rejected {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23514" || token != "" {
					t.Fatal("写入失败未拒绝签发会话", err)
				}
			} else if tc.disabled && !tc.owner {
				if !errors.Is(err, ErrInvalid) || token != "" {
					t.Fatal("禁用成员仍可签发会话", err)
				}
			} else if err != nil || len(token) != 64 {
				t.Fatal("会话签发失败", err)
			}
			var count int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE email=$1`, email).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if tc.disabled && !tc.owner || tc.rejected {
				if count != 0 {
					t.Fatal("拒绝签发后仍留下了会话", count)
				}
			} else {
				var valid bool
				if err := db.pool.QueryRow(ctx, `SELECT token_hash<>$2 AND subject=$3 AND email=$4 AND name='管理员' AND user_agent='synthetic-agent' AND expires_at>clock_timestamp() FROM admin_sessions WHERE token_hash=$1`, hash(token), token, identity.Subject, email).Scan(&valid); err != nil || !valid || count != 1 {
					t.Fatal("会话字段、哈希存储或有效期不正确", valid, count, err)
				}
			}
		})
	}
}

func TestCreateAdminSessionUsesMembershipCommittedDuringLockWait(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "等待期间禁用"
		if enabled {
			name = "等待期间启用"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			email := "session-lock-" + randomToken()[:8] + "@example.test"
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email,role,enabled) VALUES($1,'reviewer',$2)`, email, !enabled); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT email FROM admin_members WHERE email=$1 FOR UPDATE`, email); err != nil {
				t.Fatal(err)
			}
			type result struct {
				token string
				err   error
			}
			done := make(chan result, 1)
			go func() {
				token, err := (&Service{store: db}).CreateAdminSession(ctx, AdminIdentity{Subject: "synthetic-subject", Email: email})
				done <- result{token, err}
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if _, err := blocker.Exec(ctx, `UPDATE admin_members SET enabled=$2 WHERE email=$1`, email, enabled); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if enabled {
				if got.err != nil || len(got.token) != 64 {
					t.Fatal("未按锁后启用状态签发会话", got.err)
				}
			} else if !errors.Is(got.err, ErrInvalid) || got.token != "" {
				t.Fatal("等待期间已禁用的成员仍可签发会话", got.err)
			}
			var count int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE email=$1`, email).Scan(&count); err != nil || enabled && count != 1 || !enabled && count != 0 {
				t.Fatal("会话记录与锁后成员状态不一致", count, err)
			}
		})
	}
}

func TestCreateAdminSessionStartsLifetimeAfterMembershipLock(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	email := "session-lifetime-" + randomToken()[:8] + "@example.test"
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email,role,enabled) VALUES($1,'reviewer',true)`, email); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT email FROM admin_members WHERE email=$1 FOR UPDATE`, email); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := (&Service{store: db}).CreateAdminSession(ctx, AdminIdentity{Subject: "synthetic-subject", Email: email})
		done <- struct {
			token string
			err   error
		}{token, err}
	}()
	waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
	time.Sleep(3 * time.Second)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	var expires time.Time
	if err := db.pool.QueryRow(ctx, `SELECT expires_at FROM admin_sessions WHERE token_hash=$1`, hash(got.token)).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(expires); remaining < 8*time.Hour-1500*time.Millisecond {
		t.Fatalf("管理员会话有效期从锁等待前开始，剩余 %s，预期接近 %s", remaining, 8*time.Hour)
	}
}
