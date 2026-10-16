package account

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminTokenWriteTrace struct{ queries atomic.Int64 }

func (trace *adminTokenWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	return ctx
}

func (*adminTokenWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminTokenReplacementUsesTwoQueries(t *testing.T) {
	for _, action := range []string{"commit", "rollback"} {
		t.Run(action, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			a := &Service{store: db}
			const email = "replace-token@example.test"
			source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
			if err != nil {
				t.Fatal(err)
			}
			ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
			old := []string{AdminTokenPrefix + randomToken(), AdminTokenPrefix + randomToken()}
			other := AdminTokenPrefix + randomToken()
			for _, token := range append(append([]string(nil), old...), other) {
				owner := email
				if token == other {
					owner = "other-token@example.test"
				}
				if _, err := db.pool.Exec(ctx, `INSERT INTO admin_tokens(hash,email,last4) VALUES($1,$2,$3)`, hash(token), owner, token[len(token)-4:]); err != nil {
					t.Fatal(err)
				}
			}
			trace := &adminTokenWriteTrace{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			trace.queries.Store(0)
			token, info, err := issueAdminToken(ctx, tx, email)
			if err != nil {
				t.Fatal(err)
			}
			if queries := trace.queries.Load(); queries != 2 {
				t.Errorf("令牌签发用了 %d 次数据库请求，期望 2 次（不含外层事务）", queries)
			}
			var stored adminTokenInfo
			if err = tx.QueryRow(ctx, `SELECT last4,created_at,expires_at FROM admin_tokens WHERE hash=$1 AND email=$2`, hash(token), email).Scan(&stored.Last4, &stored.CreatedAt, &stored.ExpiresAt); err != nil {
				t.Fatal(err)
			}
			if len(token) != adminTokenLength || info.Last4 != token[len(token)-4:] || stored.Last4 != info.Last4 || !stored.CreatedAt.Equal(info.CreatedAt) || !stored.ExpiresAt.Equal(info.ExpiresAt) || info.ExpiresAt.Sub(info.CreatedAt) != adminTokenLifetime {
				t.Fatal("签发元数据与数据库记录或令牌有效期不一致")
			}
			if action == "commit" {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range append(append([]string(nil), old...), token, other) {
				wantValid := candidate == other || (action == "commit" && candidate == token) || (action == "rollback" && candidate != token)
				wantEmail := email
				if candidate == other {
					wantEmail = "other-token@example.test"
				}
				identity, err := a.AdminTokenIdentity(ctx, candidate)
				if wantValid && (err != nil || identity.Email != wantEmail) {
					t.Fatal("应保留的令牌已失效", err)
				}
				if !wantValid && err != ErrInvalid {
					t.Fatal("应撤销或回滚的令牌仍然有效", err)
				}
			}
		})
	}
}

func TestAdminTokenReplacementInvalidSourceDoesNotWrite(t *testing.T) {
	for _, state := range []string{"missing", "unknown", "email", "subject", "expired"} {
		t.Run(state, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			a := &Service{store: db}
			email := randomToken()[:16] + "@example.test"
			token := AdminTokenPrefix + randomToken()
			if _, err := db.pool.Exec(ctx, `INSERT INTO admin_tokens(hash,email,last4) VALUES($1,$2,$3)`, hash(token), email, token[len(token)-4:]); err != nil {
				t.Fatal(err)
			}
			identity := AdminIdentity{Subject: "g", Email: email}
			if state == "email" {
				identity.Email = "other-source@example.test"
			} else if state == "subject" {
				identity.Subject = "other"
			}
			source, err := a.CreateAdminSession(ctx, identity)
			if err != nil {
				t.Fatal(err)
			}
			if state == "expired" {
				if _, err := db.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, hash(source)); err != nil {
					t.Fatal(err)
				}
			} else if state == "missing" {
				source = ""
			} else if state == "unknown" {
				source = randomToken()
			}
			ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
			tx, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, _, err := issueAdminToken(ctx, tx, email); !errors.Is(err, ErrInvalid) {
				t.Fatal("无效来源会话未被拒绝", err)
			}
			// 直接提交，验证拒绝路径自身没有修改令牌，而非依赖调用方回滚。
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := a.AdminTokenIdentity(ctx, token); err != nil {
				t.Fatal("无效来源会话破坏了旧令牌", err)
			}
			var count int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_tokens WHERE email=$1`, email).Scan(&count); err != nil || count != 1 {
				t.Fatal("无效来源会话写入了令牌", count, err)
			}
		})
	}
}

func TestAdminTokenReplacementChecksExpiryAfterSourceLock(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a := &Service{store: db}
	email := randomToken()[:16] + "@example.test"
	source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
	block, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(context.Background())
	if _, err := block.Exec(ctx, `SELECT 1 FROM admin_sessions WHERE token_hash=$1 FOR UPDATE`, hash(source)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx.Rollback(context.Background())
		_, _, err = issueAdminToken(ctx, tx, email)
		if err != nil {
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	waitForAdminDatabaseLock(t, db, block.Conn().PgConn().PID())
	// 到期时间落在签发事务开始之后、释放来源行锁之前，必须用锁后的实时时钟判断。
	if _, err := block.Exec(ctx, `UPDATE admin_sessions SET expires_at=(
 SELECT max(xact_start)+interval '1 microsecond' FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
) WHERE token_hash=$2`, int32(block.Conn().PgConn().PID()), hash(source)); err != nil {
		t.Fatal(err)
	}
	if err := block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalid) {
		t.Fatal("等待来源会话行锁期间过期后仍能签发", err)
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_tokens WHERE email=$1`, email).Scan(&count); err != nil || count != 0 {
		t.Fatal("过期来源会话仍留下令牌", count, err)
	}
}

func TestAdminTokenReplacementStartsLifetimeAfterSourceLock(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	email := randomToken()[:16] + "@example.test"
	source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
	block, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(context.Background())
	if _, err := block.Exec(ctx, `SELECT 1 FROM admin_sessions WHERE token_hash=$1 FOR UPDATE`, hash(source)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		info adminTokenInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer tx.Rollback(context.Background())
		_, info, err := issueAdminToken(ctx, tx, email)
		if err == nil {
			err = tx.Commit(ctx)
		}
		done <- result{info: info, err: err}
	}()
	waitForAdminDatabaseLock(t, db, block.Conn().PgConn().PID())
	time.Sleep(3 * time.Second)
	if err := block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if remaining := time.Until(got.info.ExpiresAt); remaining < adminTokenLifetime-1500*time.Millisecond {
		t.Fatalf("PAT 有效期从锁等待前开始，剩余 %s，预期接近 %s", remaining, adminTokenLifetime)
	}
}

func TestAdminTokenReplacementMissingInsertResult(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	a := &Service{store: db}
	email := randomToken()[:16] + "@example.test"
	source, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithAdminSession(WithAdminAccess(ctx, AdminAccess{Actor: "google:g:" + email, Email: email}), source)
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// 在事务内安装吞掉插入的触发器，确保缺失写入结果仍作为数据库错误处理。
	if _, err := tx.Exec(ctx, `CREATE FUNCTION skip_admin_token_insert() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END';
CREATE TRIGGER skip_admin_token_insert BEFORE INSERT ON admin_tokens FOR EACH ROW EXECUTE FUNCTION skip_admin_token_insert()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := issueAdminToken(ctx, tx, email); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("有效来源缺失写入结果时未保留数据库错误", err)
	}
}
