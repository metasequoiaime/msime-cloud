package account

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoginChallengeConsumptionRoundTrips(t *testing.T) {
	for _, mode := range []string{"new", "returning", "link", "new-profile", "returning-profile", "link-profile"} {
		t.Run(mode, func(t *testing.T) {
			db := testStore(t)
			ctx := t.Context()
			identity := Identity{"google", "consume@example.test"}
			challenge := Challenge{IDHash: hash(randomToken()), Provider: identity.Provider, Subject: identity.Subject}
			wantID, wantQueries := "", 6
			switch strings.TrimSuffix(mode, "-profile") {
			case "returning":
				wantID = complete(t, db, identity).User.ID
				wantQueries = 7
			case "link":
				wantID = complete(t, db, Identity{"email", "link-owner@example.test"}).User.ID
				challenge.LinkUser = wantID
				wantQueries = 7
			}
			var grant *providerGrant
			if strings.HasSuffix(mode, "-profile") {
				grant = &providerGrant{
					Profile:       &providerProfile{Email: "profile@example.test", EmailVerified: true, Name: "合成昵称", Picture: "https://example.test/picture.jpg"},
					SealedRefresh: []byte("synthetic-sealed-refresh"), Scope: "openid profile email",
				}
			}
			if err := db.PutChallenge(ctx, challenge); err != nil {
				t.Fatal(err)
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
			tokens, err := store.completeWith(ctx, challenge, identity, grant)
			if err != nil {
				t.Fatal(err)
			}
			trace.mu.Lock()
			queries := trace.seen
			trace.mu.Unlock()
			if queries != wantQueries {
				t.Errorf("完成登录用了 %d 次数据库调用，期望 %d 次（含事务与锁）", queries, wantQueries)
			}
			if tokens.User.ID == "" || (wantID != "" && tokens.User.ID != wantID) {
				t.Fatal("登录关联了错误的账号")
			}
			if grant != nil {
				if tokens.User.DisplayName != grant.Profile.Name || tokens.User.Email != grant.Profile.Email || tokens.User.googlePicture != grant.Profile.Picture {
					t.Fatal("签发会话返回的资料没有包含本次登录更新")
				}
				var sealed, scope string
				if err = db.pool.QueryRow(ctx, `SELECT convert_from(refresh_token,'UTF8'),scope FROM auth_provider_tokens WHERE provider=$1 AND subject=$2`, identity.Provider, identity.Subject).Scan(&sealed, &scope); err != nil || sealed != string(grant.SealedRefresh) || scope != grant.Scope {
					t.Fatal("服务商刷新令牌未随会话提交", err)
				}
			}
			if principal, err := db.Authenticate(ctx, tokens.AccessToken); err != nil || principal.UserID != tokens.User.ID {
				t.Fatal("登录未创建有效会话", err)
			}
			var linked string
			var remaining int
			if err = db.pool.QueryRow(ctx, `SELECT user_id,(SELECT count(*) FROM auth_challenges WHERE id_hash=$3) FROM auth_identities WHERE provider=$1 AND subject=$2`, identity.Provider, identity.Subject, challenge.IDHash).Scan(&linked, &remaining); err != nil || linked != tokens.User.ID || remaining != 0 {
				t.Fatal("身份绑定或挑战消费不正确", remaining, err)
			}
		})
	}
}

func TestLoginChallengeConcurrentConsumption(t *testing.T) {
	db := testStore(t)
	ctx := t.Context()
	identity := Identity{"email", "consume-race@example.test"}
	challenge := Challenge{IDHash: hash(randomToken()), Provider: identity.Provider, Subject: identity.Subject}
	if err := db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	start, done := make(chan struct{}), make(chan error, workers)
	for range workers {
		go func() {
			<-start
			_, err := db.Complete(ctx, challenge, identity)
			done <- err
		}()
	}
	close(start)
	succeeded := 0
	for range workers {
		switch err := <-done; {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInvalid):
		default:
			t.Error(err)
		}
	}
	var users, sessions, challenges int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_users),(SELECT count(*) FROM auth_sessions),(SELECT count(*) FROM auth_challenges)`).Scan(&users, &sessions, &challenges); err != nil || succeeded != 1 || users != 1 || sessions != 1 || challenges != 0 {
		t.Fatal("同一挑战的并发消费结果不正确", succeeded, users, sessions, challenges, err)
	}
}
