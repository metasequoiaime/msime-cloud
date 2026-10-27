package account

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	huaweiTestClientID = "1234567890"
	huaweiTestKeyID    = "huawei-test-key"
)

// huaweiKeys 模拟华为账号的 JWKS 端点：只应答 makeVerifiers 里写的那个地址，其余请求一律失败，这样测试同时钉住发行方配置里的公钥地址。公钥按华为线上 JWKS 的写法标成 RS256，PS256 签名的 ID Token 也用这把 RSA 公钥验签。
type huaweiKeys struct {
	key      *rsa.PrivateKey
	requests atomic.Int32
}

func newHuaweiKeys(t *testing.T) *huaweiKeys {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &huaweiKeys{key: key}
}

func (k *huaweiKeys) client(t *testing.T) *http.Client {
	return &http.Client{Transport: contractTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != "https://oauth-login.cloud.huawei.com/oauth2/v3/certs" {
			t.Errorf("unexpected key request %s %s", r.Method, r.URL)
			return nil, errors.New("unexpected request")
		}
		k.requests.Add(1)
		body, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.key.PublicKey, KeyID: huaweiTestKeyID, Algorithm: "RS256", Use: "sig"}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}
}

// sign 按 Account Kit 返回的 ID Token 形状签一个令牌：`sub` 是 UnionID，`openid` 是按应用区分的 OpenID。
func (k *huaweiKeys) sign(t *testing.T, alg jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: k.key}, (&jose.SignerOptions{}).WithHeader("kid", huaweiTestKeyID).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func huaweiClaims(nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": "https://accounts.huawei.com", "aud": huaweiTestClientID, "azp": huaweiTestClientID,
		"sub": "union-id-fixture", "openid": "open-id-fixture", "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "at_hash": "fixture",
	}
}

func TestHuaweiIDTokenVerification(t *testing.T) {
	keys := newHuaweiKeys(t)
	verifiers := makeVerifiers(t.Context(), Config{Huawei: OIDCConfig{ClientIDs: []string{"other-app", huaweiTestClientID}}}, keys.client(t))
	if len(verifiers) != 1 || len(verifiers["huawei"].(audienceVerifiers)) != 2 {
		t.Fatal("华为账号的受众配置没有生效", verifiers)
	}
	a := &Service{verifiers: verifiers}
	challenge := Challenge{Provider: "huawei", Nonce: "challenge-nonce"}
	with := func(change func(map[string]any)) map[string]any {
		claims := huaweiClaims("challenge-nonce")
		change(claims)
		return claims
	}
	for _, test := range []struct {
		name   string
		alg    jose.SignatureAlgorithm
		claims map[string]any
		valid  bool
	}{
		{"PS256（Account Kit 默认）", jose.PS256, huaweiClaims("challenge-nonce"), true},
		{"RS256", jose.RS256, huaweiClaims("challenge-nonce"), true},
		{"受众写成数组", jose.PS256, with(func(c map[string]any) { c["aud"] = []string{huaweiTestClientID} }), true},
		{"别的应用的 Client ID", jose.PS256, with(func(c map[string]any) { c["aud"] = "9999999999"; c["azp"] = "9999999999" }), false},
		{"错误发行方", jose.PS256, with(func(c map[string]any) { c["iss"] = "https://accounts.huawei.com.attacker.invalid" }), false},
		{"缺省 nonce「default」", jose.PS256, with(func(c map[string]any) { c["nonce"] = "default" }), false},
		{"别的 challenge 的 nonce", jose.PS256, with(func(c map[string]any) { c["nonce"] = "other-nonce" }), false},
		{"过期", jose.PS256, with(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), false},
		{"缺签发时间", jose.PS256, with(func(c map[string]any) { delete(c, "iat") }), false},
		{"空 subject", jose.PS256, with(func(c map[string]any) { c["sub"] = "" }), false},
		{"未列入的算法", jose.PS384, huaweiClaims("challenge-nonce"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity, grant, err := a.identity(t.Context(), challenge, keys.sign(t, test.alg, test.claims))
			if !test.valid {
				if err != ErrInvalid {
					t.Fatal("接受了不合法的华为账号 ID Token", identity, err)
				}
				return
			}
			// 身份按 `sub`（UnionID）记，不用 `openid`；华为账号不带资料，也没有要保存的刷新令牌。
			if err != nil || identity != (Identity{"huawei", "union-id-fixture"}) || grant != nil {
				t.Fatal(identity, grant, err)
			}
		})
	}
	if keys.requests.Load() == 0 {
		t.Fatal("没有从华为账号的 JWKS 地址取公钥")
	}
	// 另一把密钥签的令牌即使声明全对也不能过。
	stranger := newHuaweiKeys(t)
	if _, _, err := a.identity(t.Context(), challenge, stranger.sign(t, jose.PS256, huaweiClaims("challenge-nonce"))); err != ErrInvalid {
		t.Fatal("接受了别的密钥签的令牌", err)
	}
	// 华为账号的令牌不能拿去完成 Google 或 Apple 的 challenge。
	token := keys.sign(t, jose.PS256, huaweiClaims("challenge-nonce"))
	for _, provider := range []string{"google", "apple"} {
		if _, _, err := a.identity(t.Context(), Challenge{Provider: provider, Nonce: "challenge-nonce"}, token); err != ErrInvalid {
			t.Fatal(provider, err)
		}
	}
}

func TestHuaweiIDTokenLoginHTTP(t *testing.T) {
	db := testStore(t)
	keys := newHuaweiKeys(t)
	config := Config{Huawei: OIDCConfig{ClientIDs: []string{huaweiTestClientID}}, Email: MailConfig{From: "test@example.test"}}
	a := &Service{store: db, config: config, verifiers: makeVerifiers(t.Context(), config, keys.client(t))}
	mux := http.NewServeMux()
	Mount(mux, a)
	var providers struct {
		Providers map[string]bool `json:"providers"`
	}
	if err := json.Unmarshal(apiRequest(t, mux, "GET", "/v1/auth/providers", "", "", 200).Body.Bytes(), &providers); err != nil || !providers.Providers["huawei"] || providers.Providers["google"] {
		t.Fatal(providers, err)
	}
	signIn := func(purpose, session string, status int) Tokens {
		t.Helper()
		w := apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"huawei","purpose":"`+purpose+`"}`, session, 201)
		var c struct {
			ID    string `json:"challenge_id"`
			Nonce string `json:"nonce"`
		}
		// Account Kit 只收 ^[0-9a-zA-Z:/\.\-_]{1,255}$ 的 nonce，服务端给的十六进制 nonce 可以原样传进授权请求。
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || len(c.ID) != 64 || len(c.Nonce) != 64 || strings.Trim(c.Nonce, "0123456789abcdef") != "" {
			t.Fatal(w.Body.String(), err)
		}
		raw, _ := json.Marshal(map[string]string{"challenge_id": c.ID, "credential": keys.sign(t, jose.PS256, huaweiClaims(c.Nonce))})
		w = apiRequest(t, mux, "POST", "/v1/auth/login", string(raw), session, status)
		var out Tokens
		if status == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
				t.Fatal(w.Body.String(), err)
			}
		}
		return out
	}
	first := signIn("login", "", 200)
	if again := signIn("login", "", 200); again.User.ID != first.User.ID {
		t.Fatal("同一个 UnionID 第二次登录换了账号")
	}
	_, ids, err := db.Me(t.Context(), first.User.ID)
	if err != nil || len(ids) != 1 || ids[0] != (Identity{"huawei", "union-id-fixture"}) {
		t.Fatal(ids, err)
	}
	// 已经属于别的账号的华为账号不能再绑定给另一个账号。
	other := complete(t, db, Identity{"email", "huawei-other@example.test"})
	signIn("link", other.AccessToken, 409)
}
