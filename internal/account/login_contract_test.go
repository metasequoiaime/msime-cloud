package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestProviderLoginAndIdentityLinkHTTP(t *testing.T) {
	db := testStore(t)
	sender := &captureSender{}
	t.Setenv("LOGIN_TEST_PEPPER", strings.Repeat("p", 32))
	a := &Service{store: db, sender: sender, config: Config{PepperEnv: "LOGIN_TEST_PEPPER", Google: GoogleConfig{ClientIDs: []string{"client"}}, SMS: SMSConfig{TemplateCode: "test"}, Wechat: WechatConfig{AppID: "test-app", RedirectURI: "https://example.test/callback"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "owner@example.test"})
	other := complete(t, db, Identity{"email", "other@example.test"})
	challenge := func(provider, purpose, token string) map[string]string {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"provider": provider, "purpose": purpose})
		w := apiRequest(t, mux, "POST", "/v1/auth/challenges", string(raw), token, 201)
		var c struct {
			ID    string `json:"challenge_id"`
			Nonce string `json:"nonce"`
			URL   string `json:"authorization_url"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || len(c.ID) != 64 || len(c.Nonce) != 64 {
			t.Fatal(w.Body.String(), err)
		}
		return map[string]string{"id": c.ID, "nonce": c.Nonce, "url": c.URL}
	}
	login := func(c map[string]string, token string, status int) Tokens {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"challenge_id": c["id"], "credential": "signed-token"})
		w := apiRequest(t, mux, "POST", "/v1/auth/login", string(raw), token, status)
		var out Tokens
		if status == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
				t.Fatal("missing session", err)
			}
		}
		return out
	}
	bind := func(c map[string]string, subject string) {
		a.verifiers = map[string]Verifier{"google": contractVerifier(func(context.Context, string) (*oidc.IDToken, error) {
			return &oidc.IDToken{Subject: subject, Nonce: c["nonce"], IssuedAt: time.Now()}, nil
		})}
	}
	c := challenge("google", "login", "")
	bind(c, "google-person")
	google := login(c, "", 200)
	if google.User.ID == owner.User.ID {
		t.Fatal("unrelated identities merged")
	}
	login(c, "", 401)
	c = challenge("google", "link", owner.AccessToken)
	bind(c, "linked-person")
	login(c, "", 401)
	login(c, other.AccessToken, 401)
	linked := login(c, owner.AccessToken, 200)
	if linked.User.ID != owner.User.ID {
		t.Fatal("linked identity changed user")
	}
	_, ids, err := db.Me(t.Context(), owner.User.ID)
	if err != nil || len(ids) != 2 {
		t.Fatal(ids, err)
	}
	c = challenge("google", "link", owner.AccessToken)
	bind(c, "google-person")
	login(c, owner.AccessToken, 409)
	c = challenge("google", "login", "")
	bind(c, "valid-person")
	c["nonce"] = "wrong"
	login(c, "", 401)
	c = challenge("wechat", "login", "")
	u, err := url.Parse(c["url"])
	if err != nil || u.Host != "open.weixin.qq.com" || u.Query().Get("state") != c["id"] || u.Query().Get("appid") != "test-app" {
		t.Fatal(c, err)
	}
	apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"google","purpose":"bad"}`, "", 400)
	apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"google","purpose":"link"}`, "", 401)
	apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"phone","target":"invalid"}`, "", 400)
	w := apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"phone","target":"+12025550123"}`, "", 201)
	var phone struct {
		ID string `json:"challenge_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &phone); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"challenge_id": phone.ID, "credential": sender.code})
	apiRequest(t, mux, "POST", "/v1/auth/login", string(raw), "", 200)
	apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"phone","target":"+12025550123"}`, "", 429)
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_sessions SET created_at=now()-interval '15 minutes' WHERE user_id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "DELETE", "/v1/users/me", "", owner.AccessToken, 403)
	apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"google","purpose":"link"}`, owner.AccessToken, 403)
	apiRequest(t, mux, "GET", "/v1/users/me", "", owner.AccessToken, 200)
}

// 匿名开户:客户端自带 subject 和口令,首次登录即建号,同一对凭据取回同一个账号,换一个口令就是另一个人。
func TestAnonymousAccountsAreCreatedAndReusedByCredential(t *testing.T) {
	db := testStore(t)
	t.Setenv("ANON_TEST_PEPPER", strings.Repeat("p", 32))
	a := &Service{store: db, config: Config{
		PepperEnv: "ANON_TEST_PEPPER",
		Anonymous: AnonymousConfig{Enabled: true, DailyPerAddress: 10},
	}}
	mux := http.NewServeMux()
	Mount(mux, a)

	const subject = "msime-abcdef123456"
	const secret = "0123456789abcdef0123456789abcdef"

	login := func(target, credential string, status int) Tokens {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"provider": "anonymous", "target": target})
		w := apiRequest(t, mux, "POST", "/v1/auth/challenges", string(raw), "", 201)
		var c struct {
			ID string `json:"challenge_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || len(c.ID) != 64 {
			t.Fatal(w.Body.String(), err)
		}
		raw, _ = json.Marshal(map[string]string{"challenge_id": c.ID, "credential": credential})
		w = apiRequest(t, mux, "POST", "/v1/auth/login", string(raw), "", status)
		var out Tokens
		if status == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
				t.Fatal("missing session", err)
			}
		}
		return out
	}

	first := login(subject, secret, 200)
	again := login(subject, secret, 200)
	if first.AccessToken == "" || again.AccessToken == "" {
		t.Fatal("anonymous login produced no session")
	}
	// 同一对凭据必须回到同一个账号,否则每次启动都会换一个人,云端词库跟着丢。
	me := func(token string) string {
		t.Helper()
		w := apiRequest(t, mux, "GET", "/v1/users/me", "", token, 200)
		var profile struct {
			User User `json:"user"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil || profile.User.ID == "" {
			t.Fatal(w.Body.String(), err)
		}
		return profile.User.ID
	}
	if me(first.AccessToken) != me(again.AccessToken) {
		t.Fatal("same credential produced a different account")
	}
	// 口令不同就是另一个身份。
	if me(login(subject, strings.Repeat("z", 32), 200).AccessToken) == me(first.AccessToken) {
		t.Fatal("a different secret reached the same account")
	}
	// 太短的口令不收。走的是 ErrInvalid,和其它凭据不合格一样报 401,不是参数错误。
	login(subject, "short", 401)
	// subject 的字符集在 challenge 阶段就把关,压根走不到 login。
	raw, _ := json.Marshal(map[string]string{"provider": "anonymous", "target": "BAD SUBJECT"})
	apiRequest(t, mux, "POST", "/v1/auth/challenges", string(raw), "", 400)
}

func TestAnonymousInvalidTargetDoesNotConsumeDailyLimit(t *testing.T) {
	db := testStore(t)
	t.Setenv("ANON_INVALID_TARGET_PEPPER", strings.Repeat("p", 32))
	a := &Service{store: db, config: Config{
		PepperEnv: "ANON_INVALID_TARGET_PEPPER",
		Anonymous: AnonymousConfig{Enabled: true, DailyPerAddress: 1},
	}}
	mux := http.NewServeMux()
	Mount(mux, a)
	challenge := func(target string, expected int) int {
		raw, _ := json.Marshal(map[string]string{"provider": "anonymous", "target": target})
		return apiRequest(t, mux, "POST", "/v1/auth/challenges", string(raw), "", expected).Code
	}
	if got := challenge("INVALID TARGET", http.StatusBadRequest); got != http.StatusBadRequest {
		t.Fatalf("invalid target status=%d", got)
	}
	if got := challenge("msime-valid-0001", http.StatusCreated); got != http.StatusCreated {
		t.Fatalf("valid target was blocked after invalid request: status=%d", got)
	}
}
