package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLoginRejectsAccountDeletedDuringCompletion(t *testing.T) {
	for _, mode := range []string{"已有身份登录", "绑定新身份"} {
		t.Run(mode, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			identity := Identity{"email", "delete-login@example.test"}
			user := complete(t, db, identity)
			challenge := Challenge{IDHash: hash(randomToken()), Provider: identity.Provider, Subject: identity.Subject}
			if mode == "绑定新身份" {
				identity = Identity{"google", "delete-link@example.test"}
				challenge.Provider, challenge.Subject, challenge.LinkUser = identity.Provider, identity.Subject, user.User.ID
			}
			if err := db.PutChallenge(ctx, challenge); err != nil {
				t.Fatal(err)
			}
			deletion, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer deletion.Rollback(context.Background())
			// 注销先锁用户；等登录确实等待此锁，再执行真实外键级联删除。
			if _, err := deletion.Exec(ctx, `SELECT id FROM auth_users WHERE id=$1 FOR UPDATE`, user.User.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := db.Complete(ctx, challenge, identity)
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, deletion.Conn().PgConn().PID())
			if _, err := deletion.Exec(ctx, `DELETE FROM auth_users WHERE id=$1`, user.User.ID); err != nil {
				t.Errorf("注销与完成登录发生锁冲突：%v", err)
				_ = deletion.Rollback(context.Background())
			} else if err := deletion.Commit(ctx); err != nil {
				t.Errorf("注销提交失败：%v", err)
			}
			if err := <-done; !errors.Is(err, ErrInvalid) {
				t.Errorf("账号已注销时应拒绝完成登录，实际返回：%v", err)
			}
			var users, identities, sessions int
			if err := db.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM auth_users WHERE id=$1),
 (SELECT count(*) FROM auth_identities WHERE user_id=$1),
 (SELECT count(*) FROM auth_sessions WHERE user_id=$1)`, user.User.ID).Scan(&users, &identities, &sessions); err != nil {
				t.Fatal(err)
			}
			if users != 0 || identities != 0 || sessions != 0 {
				t.Errorf("注销后仍保留账号、身份或会话：%d/%d/%d", users, identities, sessions)
			}
		})
	}
}
