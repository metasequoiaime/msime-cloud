package account

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestAppleWebFailedCallbackCancellationInvalidatesChallenge(t *testing.T) {
	f := newAppleWebFixture(t)
	verifier := newVerifier()
	state, nonce, w := f.begin("app.msime.android", "login", "", verifier)
	if w.Code != 201 {
		t.Fatal("创建挑战失败", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.a.verifiers["apple"] = contractVerifier(func(context.Context, string) (*oidc.IDToken, error) {
		// 模拟密钥获取或令牌验证失败时，浏览器已断开回调连接。
		cancel()
		return nil, ErrInvalid
	})
	form := url.Values{"state": {state}, "id_token": {"synthetic-invalid-token"}}
	r := httptest.NewRequest("POST", AppleCallbackPath, strings.NewReader(form.Encode())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if ctx.Err() != context.Canceled || !strings.Contains(w.Body.String(), "?error=invalid_token") {
		t.Fatal("没有触发请求取消后的失败回调", ctx.Err(), w.Body.String())
	}
	var remaining int
	if err := f.s.pool.QueryRow(t.Context(), "SELECT count(*) FROM auth_challenges WHERE id_hash=$1", hash(state)).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("失败回调取消后仍残留 %d 条挑战", remaining)
	}
	f.token = &oidc.IDToken{Subject: "synthetic-apple-cancel", Nonce: nonce, IssuedAt: time.Now()}
	f.a.verifiers["apple"] = contractVerifier(func(context.Context, string) (*oidc.IDToken, error) {
		return f.token, nil
	})
	page, grant := f.callback(url.Values{"state": {state}, "id_token": {"synthetic-valid-token"}})
	if grant != "" || strings.Contains(page.Body.String(), "href=") {
		t.Error("已失败的挑战仍能通过重放回调签发授权码", page.Body.String())
	}
	// 失败只消耗原挑战，用户重新发起的登录仍可正常完成。
	grant = f.granted("app.msime.android", "login", "", verifier, "synthetic-apple-cancel")
	w = f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": verifier})
	if w.Code != 200 {
		t.Fatal("重新发起的登录失败", w.Code, w.Body.String())
	}
}

func TestAppleWebCleanupAfterRequestEnds(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "主动取消"
		if expired {
			name = "期限已过"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			pending, issued := hash(randomToken()), hash(randomToken())
			for _, id := range []string{pending, issued} {
				if err := db.putAppleWebChallenge(t.Context(), id, "synthetic-nonce", "", "app.msime.android", pkceChallenge(newVerifier())); err != nil {
					t.Fatal(err)
				}
			}
			grantHash := hash(randomGrant())
			if err := db.grantAppleWeb(t.Context(), issued, appleWebChallenge{Subject: "synthetic-issued-subject"}, grantHash); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			}
			cancel()
			db.dropPendingAppleWebChallenge(ctx, pending)
			db.dropPendingAppleWebChallenge(ctx, issued)
			var remaining int
			if err := db.pool.QueryRow(t.Context(), "SELECT count(*) FROM auth_challenges WHERE id_hash=$1", pending).Scan(&remaining); err != nil || remaining != 0 {
				t.Error("请求结束后未清理待签发挑战", remaining, err)
			}
			// 补偿不能删掉另一回调已经签发的授权码。
			if c, err := db.redeemAppleWeb(t.Context(), grantHash); err != nil || c.Subject != "synthetic-issued-subject" {
				t.Fatal("请求结束后的清理破坏了已签发授权码", c.Subject, err)
			}
		})
	}
}
