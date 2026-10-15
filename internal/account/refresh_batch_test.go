package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRefreshSuccessUsesFourRoundTrips(t *testing.T) {
	for _, lifetime := range []string{"normal", "short"} {
		t.Run(lifetime, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			first := complete(t, db, Identity{"google", "refresh-batch"})
			if _, err := db.pool.Exec(ctx, `UPDATE auth_users SET display_name='合成用户',avatar_key='avatars/synthetic.jpg' WHERE id=$1`, first.User.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `UPDATE auth_identities SET email='refresh@example.test',email_verified=true,picture='https://example.test/picture.jpg',updated_at=now() WHERE user_id=$1`, first.User.ID); err != nil {
				t.Fatal(err)
			}
			wantUser, _, err := db.Me(ctx, first.User.ID)
			if err != nil {
				t.Fatal(err)
			}
			principal, err := db.Authenticate(ctx, first.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			maxLifetime := 900
			if lifetime == "short" {
				maxLifetime = 120
				if _, err := db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=clock_timestamp()+interval '2 minutes' WHERE id=$1`, principal.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if err = pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			trace.seen, trace.statements = 0, nil
			trace.mu.Unlock()
			store := &Store{pool: pool}
			next, err := store.Refresh(ctx, first.RefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != 4 {
				t.Errorf("成功刷新用了 %d 次数据库请求，期望 4 次（含事务和会话锁）", queries)
			}
			if next.User.ID != wantUser.ID || next.User.DisplayName != wantUser.DisplayName || !next.User.CreatedAt.Equal(wantUser.CreatedAt) || next.User.avatarKey != wantUser.avatarKey || next.User.Email != wantUser.Email || next.User.googlePicture != wantUser.googlePicture {
				t.Fatal("刷新返回的用户资料与完整资料查询不一致")
			}
			if len(next.AccessToken) != 64 || len(next.RefreshToken) != 64 || next.AccessToken == first.AccessToken || next.RefreshToken == first.RefreshToken || next.TokenType != "Bearer" || next.ExpiresIn <= 0 || next.ExpiresIn > maxLifetime {
				t.Fatal("轮换结果或有效期不正确")
			}
			if _, err = db.Authenticate(ctx, first.AccessToken); !errors.Is(err, ErrInvalid) {
				t.Fatal("旧访问令牌没有失效", err)
			}
			if got, err := db.Authenticate(ctx, next.AccessToken); err != nil || got.SessionID != principal.SessionID || got.UserID != wantUser.ID {
				t.Fatal("新访问令牌没有关联原会话", err)
			}
			var history int
			var bounded bool
			if err = db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_used_refresh WHERE hash=$1 AND session_id=$2 AND used_at IS NOT NULL),access_expires<=expires_at FROM auth_sessions WHERE id=$2`, hash(first.RefreshToken), principal.SessionID).Scan(&history, &bounded); err != nil || history != 1 || !bounded {
				t.Fatal("刷新历史或会话有效期边界不正确", history, bounded, err)
			}
		})
	}
}

func TestRefreshUsesStateCommittedDuringSessionLockWait(t *testing.T) {
	for _, rotated := range []bool{true, false} {
		name := "等待期间已轮换"
		if !rotated {
			name = "等待期间已撤销"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			first := complete(t, db, Identity{"email", "refresh-lock@example.test"})
			principal, err := db.Authenticate(ctx, first.AccessToken)
			if err != nil {
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
			wantAccess, wantRefresh := hash(first.AccessToken), hash(first.RefreshToken)
			wantHistory := 0
			wantErr := ErrInvalid
			if rotated {
				wantAccess, wantRefresh = hash(randomToken()), hash(randomToken())
				wantHistory, wantErr = 1, ErrRefreshSuperseded
				if _, err := blocker.Exec(ctx, `INSERT INTO auth_used_refresh(hash,session_id,used_at) VALUES($1,$2,clock_timestamp())`, hash(first.RefreshToken), principal.SessionID); err != nil {
					t.Fatal(err)
				}
				_, err = blocker.Exec(ctx, `UPDATE auth_sessions SET access_hash=$2,refresh_hash=$3 WHERE id=$1`, principal.SessionID, wantAccess, wantRefresh)
			} else {
				_, err = blocker.Exec(ctx, `UPDATE auth_sessions SET revoked=true WHERE id=$1`, principal.SessionID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, wantErr) {
				t.Fatal("未按等待期间提交的会话状态处理", err)
			}
			var valid bool
			if err := db.pool.QueryRow(ctx, `SELECT access_hash=$2 AND refresh_hash=$3 AND revoked=$4 AND (SELECT count(*) FROM auth_used_refresh WHERE session_id=$1)=$5 FROM auth_sessions WHERE id=$1`, principal.SessionID, wantAccess, wantRefresh, !rotated, wantHistory).Scan(&valid); err != nil || !valid {
				t.Fatal("等待期间提交的轮换或撤销状态被覆盖", valid, err)
			}
		})
	}
}
