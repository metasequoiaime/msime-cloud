package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConcurrentLinkedIdentityLoginsUpdateDefaultName(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	identities := []Identity{{"google", "linked-google"}, {"apple", "linked-apple"}}
	user := complete(t, db, identities[0])
	if _, err := db.pool.Exec(ctx, `INSERT INTO auth_identities(provider,subject,user_id) VALUES($1,$2,$3)`, identities[1].Provider, identities[1].Subject, user.User.ID); err != nil {
		t.Fatal(err)
	}
	challenges := make([]Challenge, len(identities))
	for i, identity := range identities {
		challenges[i] = Challenge{IDHash: hash(randomToken()), Provider: identity.Provider}
		if err := db.PutChallenge(ctx, challenges[i]); err != nil {
			t.Fatal(err)
		}
	}
	blocker, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	// 暂停身份资料写入，让两个登录都进入事务；普通身份查询仍可继续。
	if _, err := blocker.Exec(ctx, `LOCK TABLE auth_identities IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		tokens Tokens
		err    error
	}
	results := make(chan result, len(identities))
	for i, identity := range identities {
		go func() {
			tokens, err := db.completeWith(ctx, challenges[i], identity, &providerGrant{Profile: &providerProfile{Name: "身份昵称", Email: identity.Provider + "@example.test"}})
			results <- result{tokens, err}
		}()
	}
	// 修复前两个登录都等待身份表；修复后第二个登录间接等待第一个持有的用户行锁。
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := db.pool.QueryRow(ctx, `WITH RECURSIVE blocked(pid) AS (
 SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
 UNION SELECT a.pid FROM pg_stat_activity a JOIN blocked b ON b.pid=ANY(pg_blocking_pids(a.pid))
) SELECT count(*) FROM blocked`, int32(blocker.Conn().PgConn().PID())).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == len(identities) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("两个登录没有同时进入锁等待")
		case r := <-results:
			t.Fatal("登录在身份表解锁前结束", r.err)
		case <-ticker.C:
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range identities {
		r := <-results
		if r.err != nil {
			t.Errorf("并发身份登录失败：%v", r.err)
			continue
		}
		if r.tokens.User.ID != user.User.ID || r.tokens.User.DisplayName != "身份昵称" {
			t.Errorf("并发登录返回错误的账号或昵称：%+v", r.tokens.User)
		}
		if _, err := db.Authenticate(ctx, r.tokens.AccessToken); err != nil {
			t.Errorf("并发登录签发的会话无效：%v", err)
		}
	}
	for _, challenge := range challenges {
		if _, err := db.Complete(ctx, challenge, identities[0]); !errors.Is(err, ErrInvalid) {
			t.Errorf("成功登录的挑战未被消费：%v", err)
		}
	}
}
