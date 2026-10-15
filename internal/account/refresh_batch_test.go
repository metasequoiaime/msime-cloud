package account

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRefreshSuccessUsesFiveRoundTrips(t *testing.T) {
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
			if queries != 5 {
				t.Errorf("成功刷新用了 %d 次数据库请求，期望 5 次（含事务和会话锁）", queries)
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
