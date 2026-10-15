package account

import (
	"context"
	"errors"
	"testing"
)

func TestLoginSessionBatchFailureRollsBackEarlierWrites(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	identity := Identity{"google", "batch-rollback"}
	challenge := Challenge{IDHash: hash(randomToken()), Provider: identity.Provider, Subject: identity.Subject}
	if err := db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	grant := &providerGrant{
		Profile:       &providerProfile{Email: "batch-rollback@example.test", EmailVerified: true, Name: "合成昵称"},
		SealedRefresh: []byte("synthetic-sealed-refresh"), Scope: "openid profile email",
	}
	// 在批次最后的会话写入制造数据库错误，前面的账号、身份和资料写入必须一起回滚。
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_sessions ADD CONSTRAINT login_batch_test_failure CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE auth_sessions DROP CONSTRAINT IF EXISTS login_batch_test_failure`); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.completeWith(ctx, challenge, identity, grant); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatal("会话约束失败没有返回数据库错误", err)
	}
	var challenges, users, identities, providerTokens, sessions int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_challenges WHERE id_hash=$1),(SELECT count(*) FROM auth_users),(SELECT count(*) FROM auth_identities),(SELECT count(*) FROM auth_provider_tokens),(SELECT count(*) FROM auth_sessions)`, challenge.IDHash).Scan(&challenges, &users, &identities, &providerTokens, &sessions); err != nil || challenges != 1 || users != 0 || identities != 0 || providerTokens != 0 || sessions != 0 {
		t.Fatal("失败登录留下了部分写入或消耗了挑战", challenges, users, identities, providerTokens, sessions, err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE auth_sessions DROP CONSTRAINT login_batch_test_failure`); err != nil {
		t.Fatal(err)
	}
	if tokens, err := db.completeWith(ctx, challenge, identity, grant); err != nil || tokens.User.Email != grant.Profile.Email || tokens.User.DisplayName != grant.Profile.Name {
		t.Fatal("数据库恢复后无法用原挑战完成登录", err)
	}
}
