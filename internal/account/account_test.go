package account

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("设置 MSIME_TEST_DATABASE_URL 运行 PostgreSQL 集成测试")
	}
	if !strings.Contains(dsn, "msime_auth_test") {
		t.Fatal("仅允许使用名为 msime_auth_test 的一次性数据库")
	}
	s, e := Open(context.Background(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	if e = s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	// 只允许指向一次性测试数据库，CI 使用专用 Postgres service。
	if _, e = s.pool.Exec(context.Background(), "TRUNCATE auth_users,auth_challenges,auth_sessions,auth_identities,auth_used_refresh,auth_rates CASCADE"); e != nil {
		t.Fatal(e)
	}
	return s
}
func complete(t *testing.T, s *Store, identity Identity) Tokens {
	t.Helper()
	ctx := context.Background()
	id := randomToken()
	c := Challenge{IDHash: hash(id), Provider: identity.Provider, Subject: identity.Subject}
	if e := s.PutChallenge(ctx, c); e != nil {
		t.Fatal(e)
	}
	v, e := s.Complete(ctx, c, identity)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestSessionRotationReplayLogoutAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := complete(t, s, Identity{"email", "one@example.test"})
	p, e := s.Authenticate(ctx, first.AccessToken)
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.Refresh(ctx, first.RefreshToken)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, first.AccessToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("旧 access 令牌应失效", e)
	}
	if _, e = s.Authenticate(ctx, next.AccessToken); e != nil {
		t.Fatal(e)
	}
	// 30 秒内再次出现的旧令牌按并发刷新处理：409，不撤销，新令牌继续有效，可以重复出现。
	for range 2 {
		if _, e = s.Refresh(ctx, first.RefreshToken); !errors.Is(e, ErrRefreshSuperseded) || errors.Is(e, ErrInvalid) {
			t.Fatal("宽限期内的旧令牌应返回 refresh_superseded", e)
		}
	}
	if _, e = s.Authenticate(ctx, next.AccessToken); e != nil {
		t.Fatal("宽限期内的旧令牌不应撤销会话", e)
	}
	// 超过 30 秒后仍是重放：撤销整个会话。
	if _, e = s.pool.Exec(ctx, "UPDATE auth_used_refresh SET used_at=now()-interval '31 seconds' WHERE hash=$1", hash(first.RefreshToken)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(ctx, first.RefreshToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("刷新重放应失败", e)
	}
	if _, e = s.Authenticate(ctx, next.AccessToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("重放后应撤销会话族", e)
	}
	if _, e = s.Refresh(ctx, next.RefreshToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("撤销后新刷新令牌也应失效", e)
	}
	another := complete(t, s, Identity{"email", "one@example.test"})
	if another.User.ID != p.UserID {
		t.Fatal("重复登录不应重复建用户")
	}
	p, e = s.Authenticate(ctx, another.AccessToken)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Logout(ctx, p, true); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(ctx, another.RefreshToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("退出后不能刷新", e)
	}
	again := complete(t, s, Identity{"email", "one@example.test"})
	if e = s.DeleteUser(ctx, p.UserID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(ctx, again.AccessToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("注销后访问应失败", e)
	}
	var n int
	if e = s.pool.QueryRow(ctx, "SELECT count(*) FROM auth_identities").Scan(&n); e != nil || n != 0 {
		t.Fatal("注销应清理身份", n, e)
	}
}

// 并发刷新：同一个刷新令牌同时提交多次，只有一次轮换成功，其余在宽限期内得到 refresh_superseded，会话不被撤销。迁移前轮换、used_at 为空的旧行按超过宽限期处理。会话已退出时，宽限期内的旧令牌也只得到 401。
func TestRefreshSupersededGrace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := complete(t, s, Identity{"email", "race@example.test"})
	results := make(chan error, 6)
	tokens := make(chan Tokens, 6)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.Refresh(ctx, first.RefreshToken)
			results <- e
			if e == nil {
				tokens <- v
			}
		}()
	}
	wg.Wait()
	close(results)
	close(tokens)
	won, superseded := 0, 0
	for e := range results {
		switch {
		case e == nil:
			won++
		case errors.Is(e, ErrRefreshSuperseded):
			superseded++
		default:
			t.Fatal("unexpected refresh error", e)
		}
	}
	if won != 1 || superseded != 5 {
		t.Fatal("won", won, "superseded", superseded)
	}
	winner := <-tokens
	if _, e := s.Authenticate(ctx, winner.AccessToken); e != nil {
		t.Fatal("并发刷新不应撤销会话", e)
	}
	// 迁移前轮换的行没有 used_at，按重放处理。
	if _, e := s.pool.Exec(ctx, "UPDATE auth_used_refresh SET used_at=NULL WHERE hash=$1", hash(first.RefreshToken)); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Refresh(ctx, first.RefreshToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("没有轮换时间的旧令牌应按重放处理", e)
	}
	if _, e := s.Authenticate(ctx, winner.AccessToken); !errors.Is(e, ErrInvalid) {
		t.Fatal("重放应撤销会话", e)
	}
	// 已退出的会话：宽限期内的旧令牌返回 401 而不是 409。
	second := complete(t, s, Identity{"email", "race@example.test"})
	p, e := s.Authenticate(ctx, second.AccessToken)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(ctx, second.RefreshToken); e != nil {
		t.Fatal(e)
	}
	if e = s.Logout(ctx, p, false); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(ctx, second.RefreshToken); !errors.Is(e, ErrInvalid) || errors.Is(e, ErrRefreshSuperseded) {
		t.Fatal("已退出会话的旧令牌应返回 invalid_credentials", e)
	}
}
func TestChallengeSingleUseAttemptsExpiryAndLinkConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := randomToken()
	c := Challenge{IDHash: hash(id), Provider: "google"}
	if e := s.PutChallenge(ctx, c); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 5; i++ {
		if _, e := s.Attempt(ctx, id); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Attempt(ctx, id); !errors.Is(e, ErrInvalid) {
		t.Fatal("超过五次应锁定", e)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.Complete(ctx, c, Identity{"google", "subject"}); e == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("挑战只能消费一次", successes.Load())
	}
	expiredID := randomToken()
	expired := Challenge{IDHash: hash(expiredID), Provider: "email"}
	s.PutChallenge(ctx, expired)
	s.pool.Exec(ctx, "UPDATE auth_challenges SET expires_at=now()-interval '1 second' WHERE id_hash=$1", expired.IDHash)
	if _, e := s.Attempt(ctx, expiredID); !errors.Is(e, ErrInvalid) {
		t.Fatal("过期挑战", e)
	}
	one := complete(t, s, Identity{"email", "one@example.test"})
	two := complete(t, s, Identity{"google", "two"})
	link := Challenge{IDHash: hash(randomToken()), Provider: "google", LinkUser: one.User.ID}
	s.PutChallenge(ctx, link)
	if _, e := s.Complete(ctx, link, Identity{"google", "two"}); !errors.Is(e, ErrConflict) {
		t.Fatal("不能夺取其他用户身份", e)
	}
	if one.User.ID == two.User.ID {
		t.Fatal("独立身份不应自动合并")
	}
	link.IDHash = hash(randomToken())
	s.PutChallenge(ctx, link)
	bound, e := s.Complete(ctx, link, Identity{"google", "new"})
	if e != nil || bound.User.ID != one.User.ID {
		t.Fatal("显式绑定失败", e)
	}
}
func TestRateIsSharedAndBounded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var n atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Rate(ctx, "target", 5, time.Hour) == nil {
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 5 {
		t.Fatal(n.Load())
	}
}

// ReleaseRateLimit 退回一次计数：名额是 1 的去重键退回后可以再占一次；没有计数或窗口过期时什么也不做，也不会减成负数。
func TestReleaseRateLimit(t *testing.T) {
	s := testStore(t)
	a := &Service{store: s}
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DELETE FROM auth_rates WHERE key LIKE 'qq-bot-gap:%'`); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseRateLimit(ctx, "qq-bot-gap", "候选框"); err != nil {
		t.Fatal(err)
	}
	if err := a.RateLimit(ctx, "qq-bot-gap", "候选框", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.RateLimit(ctx, "qq-bot-gap", "候选框", 1, time.Hour); !errors.Is(err, ErrLimited) {
		t.Fatal(err)
	}
	// 超限时计数停在上限加一，退回两次才回到 0。
	for range 2 {
		if err := a.ReleaseRateLimit(ctx, "qq-bot-gap", "候选框"); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ReleaseRateLimit(ctx, "qq-bot-gap", "候选框"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, "qq-bot-gap:"+hash("候选框")).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := a.RateLimit(ctx, "qq-bot-gap", "候选框", 1, time.Hour); err != nil {
		t.Fatal("released slot not available", err)
	}
	// 窗口过期的计数不动，下次 RateLimit 重新开窗口。
	if _, err := s.pool.Exec(ctx, `UPDATE auth_rates SET expires_at=now()-interval '1 minute' WHERE key=$1`, "qq-bot-gap:"+hash("候选框")); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseRateLimit(ctx, "qq-bot-gap", "候选框"); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count FROM auth_rates WHERE key=$1`, "qq-bot-gap:"+hash("候选框")).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestOIDCSignatureAudienceIssuerNonceAndExpiry(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if e != nil {
		t.Fatal(e)
	}
	a := &Service{verifiers: map[string]Verifier{"google": oidc.NewVerifier("https://accounts.google.com", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, &oidc.Config{ClientID: "client"})}}
	for _, test := range []struct {
		name, issuer, aud, nonce, sub string
		expiry                        int64
		valid                         bool
	}{
		{"合法", "https://accounts.google.com", "client", "nonce", "subject", time.Now().Add(time.Minute).Unix(), true},
		{"错误 audience", "https://accounts.google.com", "attacker", "nonce", "subject", time.Now().Add(time.Minute).Unix(), false},
		{"错误 issuer", "https://attacker.invalid", "client", "nonce", "subject", time.Now().Add(time.Minute).Unix(), false},
		{"错误 nonce", "https://accounts.google.com", "client", "other", "subject", time.Now().Add(time.Minute).Unix(), false},
		{"过期", "https://accounts.google.com", "client", "nonce", "subject", time.Now().Add(-time.Hour).Unix(), false},
		{"空 subject", "https://accounts.google.com", "client", "nonce", "", time.Now().Add(time.Minute).Unix(), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"iss": test.issuer, "aud": test.aud, "nonce": test.nonce, "sub": test.sub, "exp": test.expiry, "iat": time.Now().Unix()})
			signed, _ := signer.Sign(b)
			token, _ := signed.CompactSerialize()
			_, _, e := a.identity(context.Background(), Challenge{Provider: "google", Nonce: "nonce"}, token)
			if (e == nil) != test.valid {
				t.Fatal(e)
			}
		})
	}
	if _, _, e = a.identity(context.Background(), Challenge{Provider: "google", Nonce: "nonce"}, "eyJhbGciOiJub25lIn0.e30."); e == nil {
		t.Fatal("不能接受无签名令牌")
	}
}

type captureSender struct {
	code string
	fail bool
}

func (s *captureSender) Send(_ context.Context, _, _, code string) error {
	s.code = code
	if s.fail {
		return errors.New("不可用")
	}
	return nil
}
func TestEmailHTTPFlowAndFailure(t *testing.T) {
	s := testStore(t)
	t.Setenv("AUTH_TEST_PEPPER", strings.Repeat("p", 32))
	sender := &captureSender{}
	a := &Service{store: s, config: Config{PepperEnv: "AUTH_TEST_PEPPER", Email: MailConfig{From: "login@example.test"}}, sender: sender}
	mux := http.NewServeMux()
	Mount(mux, a)
	request := func(path string, v any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(v)
		r := httptest.NewRequest("POST", path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := request("/v1/auth/challenges", map[string]string{"provider": "email", "target": "one@example.test"})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), sender.code) {
		t.Fatal("响应不得泄露验证码")
	}
	var c struct {
		ID string `json:"challenge_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &c)
	if w = request("/v1/auth/login", map[string]string{"challenge_id": c.ID, "credential": "abcdef"}); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = request("/v1/auth/login", map[string]string{"challenge_id": c.ID, "credential": sender.code})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request("/v1/auth/login", map[string]string{"challenge_id": c.ID, "credential": sender.code}); w.Code != 401 {
		t.Fatal("OTP 重放应失败", w.Code)
	}
	sender.fail = true
	w = request("/v1/auth/challenges", map[string]string{"provider": "email", "target": "two@example.test"})
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	var n int
	s.pool.QueryRow(context.Background(), "SELECT count(*) FROM auth_challenges WHERE subject='two@example.test'").Scan(&n)
	if n != 0 {
		t.Fatal("失败发送应删除挑战")
	}
}
