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

func TestAppleWebLateCallbackPreservesIssuedGrant(t *testing.T) {
	for _, validToken := range []bool{true, false} {
		name := "重复有效回调"
		if !validToken {
			name = "迟到的无效令牌"
		}
		t.Run(name, func(t *testing.T) {
			f := newAppleWebFixture(t)
			verifier := newVerifier()
			state, nonce, w := f.begin("app.msime.android", "login", "", verifier)
			if w.Code != 201 {
				t.Fatal("创建挑战失败", w.Code, w.Body.String())
			}
			waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			token := &oidc.IDToken{Subject: "apple-callback-race", Nonce: nonce, IssuedAt: time.Now()}
			f.a.verifiers["apple"] = contractVerifier(func(_ context.Context, raw string) (*oidc.IDToken, error) {
				if raw == "delayed" {
					// 先确保迟到请求已经读到待签发的挑战，再让另一请求签发授权码。
					close(entered)
					select {
					case <-release:
					case <-waitCtx.Done():
						return nil, waitCtx.Err()
					}
					if !validToken {
						return nil, ErrInvalid
					}
				}
				return token, nil
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				page, _ := f.callback(url.Values{"state": {state}, "id_token": {"delayed"}})
				done <- page
			}()
			select {
			case <-entered:
			case <-waitCtx.Done():
				t.Fatal("迟到回调未进入令牌验证", waitCtx.Err())
			}
			page, grant := f.callback(url.Values{"state": {state}, "id_token": {"winner"}})
			if grant == "" {
				t.Fatal("首个完成的回调未签发授权码", page.Body.String())
			}
			close(release)
			select {
			case page = <-done:
			case <-waitCtx.Done():
				t.Fatal("迟到回调未完成", waitCtx.Err())
			}
			errorCode := "unavailable"
			if !validToken {
				errorCode = "invalid_token"
			}
			if !strings.Contains(page.Body.String(), "?error="+errorCode) {
				t.Fatal("迟到回调未返回预期错误", page.Body.String())
			}
			w = f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": verifier})
			if w.Code != 200 {
				t.Fatal("迟到回调使已签发的授权码失效", w.Code, w.Body.String())
			}
			w = f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": verifier})
			if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_grant") {
				t.Fatal("授权码被重复兑换", w.Code, w.Body.String())
			}
		})
	}
}
