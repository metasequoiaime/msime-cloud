package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRefreshReplayUsesFourRoundTrips(t *testing.T) {
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeSimpleProtocol} {
		t.Run(mode.String(), func(t *testing.T) {
			for _, state := range []string{"stale", "null_time", "revoked", "expired", "unknown", "current_revoked", "current_expired", "grace"} {
				t.Run(state, func(t *testing.T) {
					db := testStore(t)
					ctx := t.Context()
					first := complete(t, db, Identity{"email", "replay-query@example.test"})
					other := complete(t, db, Identity{"email", "replay-other@example.test"})
					principal, err := db.Authenticate(ctx, first.AccessToken)
					if err != nil {
						t.Fatal(err)
					}
					token, access := first.RefreshToken, first.AccessToken
					rotated := state != "current_revoked" && state != "current_expired"
					if rotated {
						next, err := db.Refresh(ctx, token)
						if err != nil {
							t.Fatal(err)
						}
						access = next.AccessToken
					}
					if state == "stale" {
						_, err = db.pool.Exec(ctx, `UPDATE auth_used_refresh SET used_at=clock_timestamp()-interval '1 day' WHERE hash=$1`, hash(token))
					} else if state == "null_time" {
						_, err = db.pool.Exec(ctx, `UPDATE auth_used_refresh SET used_at=NULL WHERE hash=$1`, hash(token))
					} else if state == "revoked" || state == "current_revoked" {
						_, err = db.pool.Exec(ctx, `UPDATE auth_sessions SET revoked=true WHERE id=$1`, principal.SessionID)
					} else if state == "expired" || state == "current_expired" {
						_, err = db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=clock_timestamp()-interval '1 day' WHERE id=$1`, principal.SessionID)
					} else if state == "unknown" {
						token = randomToken()
					}
					if err != nil {
						t.Fatal(err)
					}
					trace := &statementCancellation{}
					cfg := db.pool.Config()
					cfg.MaxConns = 1
					cfg.ConnConfig.Tracer = trace
					cfg.ConnConfig.DefaultQueryExecMode = mode
					pool, err := pgxpool.NewWithConfig(ctx, cfg)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(pool.Close)
					store := &Store{pool: pool}
					_, err = store.Refresh(ctx, token)
					wantErr := ErrInvalid
					if state == "grace" {
						wantErr = ErrRefreshSuperseded
					}
					if !errors.Is(err, wantErr) {
						t.Fatal("重放处理返回了错误的结果", err, wantErr)
					}
					trace.mu.Lock()
					calls := trace.seen
					trace.mu.Unlock()
					if calls != 4 {
						t.Errorf("重放处理用了 %d 次数据库请求，期望 4 次（含事务和会话锁）", calls)
					}
					wantRevoked := state == "stale" || state == "null_time" || state == "revoked" || state == "expired" || state == "current_revoked"
					var revoked bool
					if err := db.pool.QueryRow(ctx, `SELECT revoked FROM auth_sessions WHERE id=$1`, principal.SessionID).Scan(&revoked); err != nil || revoked != wantRevoked {
						t.Fatal("会话撤销状态不正确", revoked, wantRevoked, err)
					}
					_, err = db.Authenticate(ctx, access)
					if state == "grace" || state == "unknown" {
						if err != nil {
							t.Fatal("有效会话被撤销", err)
						}
					} else if !errors.Is(err, ErrInvalid) {
						t.Fatal("失效会话仍能通过认证", err)
					}
					if _, err := db.Authenticate(ctx, other.AccessToken); err != nil {
						t.Fatal("影响了其他用户的会话", err)
					}
					// 单连接池再次取用成功，确认所有失败分支都释放了事务。
					if err := pool.Ping(ctx); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestRefreshReplayRevocationFailureRollsBack(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "statement"
		if deferred {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			first := complete(t, db, Identity{"email", "replay-failure@example.test"})
			next, err := db.Refresh(ctx, first.RefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `UPDATE auth_used_refresh SET used_at=NULL WHERE hash=$1`, hash(first.RefreshToken)); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER reject_replay_revocation BEFORE UPDATE ON auth_sessions`
			if deferred {
				trigger = `CREATE CONSTRAINT TRIGGER reject_replay_revocation AFTER UPDATE ON auth_sessions DEFERRABLE INITIALLY DEFERRED`
			}
			if _, err := db.pool.Exec(ctx, `CREATE FUNCTION reject_replay_revocation() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''合成撤销失败''; END'; `+trigger+` FOR EACH ROW WHEN (NEW.revoked) EXECUTE FUNCTION reject_replay_revocation()`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_replay_revocation ON auth_sessions; DROP FUNCTION IF EXISTS reject_replay_revocation()`); err != nil {
					t.Error(err)
				}
			})
			_, err = db.Refresh(ctx, first.RefreshToken)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
				t.Fatal("撤销失败没有返回数据库错误", err)
			}
			if _, err := db.Authenticate(ctx, next.AccessToken); err != nil {
				t.Fatal("撤销失败后会话未回滚", err)
			}
		})
	}
}

func TestRefreshReplayKeepsConcurrentSessionChanges(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "rotate"
		if revoke {
			name = "revoke"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			first := complete(t, db, Identity{"email", "replay-wait@example.test"})
			next, err := db.Refresh(ctx, first.RefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			principal, err := db.Authenticate(ctx, next.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `UPDATE auth_used_refresh SET used_at=NULL WHERE hash=$1`, hash(first.RefreshToken)); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM auth_sessions WHERE id=$1 FOR UPDATE`, principal.SessionID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := db.Refresh(ctx, first.RefreshToken)
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			wantAccess, wantRefresh := hash(next.AccessToken), hash(next.RefreshToken)
			if !revoke {
				wantAccess, wantRefresh = hash(randomToken()), hash(randomToken())
			}
			var version string
			if err := blocker.QueryRow(ctx, `UPDATE auth_sessions SET access_hash=$2,refresh_hash=$3,revoked=$4 WHERE id=$1 RETURNING xmin::text`, principal.SessionID, wantAccess, wantRefresh, revoke).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, ErrInvalid) {
				t.Fatal("重放没有被拒绝", err)
			}
			var valid bool
			if err := db.pool.QueryRow(ctx, `SELECT revoked AND access_hash=$2 AND refresh_hash=$3 AND (NOT $4 OR xmin::text=$5) FROM auth_sessions WHERE id=$1`, principal.SessionID, wantAccess, wantRefresh, revoke, version).Scan(&valid); err != nil || !valid {
				t.Fatal("未保留并发更新的令牌或重复写入已撤销会话", valid, err)
			}
		})
	}
}
