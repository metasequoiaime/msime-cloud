package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// androidEditionApplicationIDs 抄自 msime 仓库 shared/contracts/editions.json 中各版本的 platforms.android.application_id（full、pinyin、wubi、japanese、vietnamese、tibetan）。那边增删版本时这里和 appleWebApps 要一起改。
var androidEditionApplicationIDs = []string{"app.msime.android", "app.msime.android.pinyin", "app.msime.android.wubi", "app.msime.android.japanese", "app.msime.android.vietnamese", "app.msime.android.tibetan"}

func TestAppleWebAppsMatchAndroidEditions(t *testing.T) {
	if len(appleWebApps) != len(androidEditionApplicationIDs) {
		t.Fatal("whitelist size", len(appleWebApps))
	}
	for _, id := range androidEditionApplicationIDs {
		if !appleWebApps[id] {
			t.Fatal("missing edition", id)
		}
	}
}

func TestPKCEHelpers(t *testing.T) {
	// RFC 7636 附录 B 的示例。
	if pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("S256 does not match RFC 7636 appendix B")
	}
}

func TestPKCEValidation(t *testing.T) {
	verifier := strings.Repeat("a", 43)
	if !validPKCEVerifier(verifier) || validPKCEVerifier(strings.Repeat("a", 42)) || validPKCEVerifier(strings.Repeat("a", 129)) || validPKCEVerifier(strings.Repeat("a", 42)+"=") {
		t.Fatal("verifier shape")
	}
	if !base64URLToken(pkceChallenge(verifier), 32) || base64URLToken("short", 32) || base64URLToken(pkceChallenge(verifier)+"=", 32) {
		t.Fatal("challenge shape")
	}
}

// appleWebFixture 是启用了 Services ID、Apple 校验器返回 token 指向内容的服务。
type appleWebFixture struct {
	t     *testing.T
	a     *Service
	s     *Store
	mux   *http.ServeMux
	token *oidc.IDToken
}

func newAppleWebFixture(t *testing.T) *appleWebFixture {
	s := testStore(t)
	f := &appleWebFixture{t: t, s: s}
	f.a = &Service{store: s, config: Config{Apple: OIDCConfig{ClientIDs: []string{"app.msime.ios", AppleServicesID}}}, verifiers: map[string]Verifier{"apple": contractVerifier(func(context.Context, string) (*oidc.IDToken, error) {
		if f.token == nil {
			return nil, ErrInvalid
		}
		return f.token, nil
	})}}
	f.mux = http.NewServeMux()
	Mount(f.mux, f.a)
	return f
}

func (f *appleWebFixture) json(path, bearer string, v any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func newVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// begin 发起一次网页登录，返回 state 和 nonce。
func (f *appleWebFixture) begin(app, purpose, bearer, verifier string) (string, string, *httptest.ResponseRecorder) {
	w := f.json("/v1/auth/apple/web", bearer, map[string]string{"code_challenge": pkceChallenge(verifier), "code_challenge_method": "S256", "app": app, "purpose": purpose})
	if w.Code != 201 {
		return "", "", w
	}
	var v struct {
		URL string `json:"authorization_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &v)
	u, err := url.Parse(v.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != AppleServicesID || q.Get("redirect_uri") != appleCallbackURL || q.Get("response_mode") != "form_post" || q.Get("response_type") != "code id_token" || q.Get("scope") != "name email" {
		f.t.Fatal("authorization url", v.URL)
	}
	return q.Get("state"), q.Get("nonce"), w
}

var grantPattern = regexp.MustCompile(`href="([a-z.]+)://auth/apple\?grant=([A-Za-z0-9_-]{43})"`)

// callback 模拟 Apple 的 form_post，返回落地页和其中的授权码。
func (f *appleWebFixture) callback(form url.Values) (*httptest.ResponseRecorder, string) {
	r := httptest.NewRequest("POST", AppleCallbackPath, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://appleid.apple.com")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	m := grantPattern.FindStringSubmatch(w.Body.String())
	if m == nil {
		return w, ""
	}
	return w, m[2]
}

func (f *appleWebFixture) granted(app, purpose, bearer, verifier, subject string) string {
	state, nonce, w := f.begin(app, purpose, bearer, verifier)
	if state == "" {
		f.t.Fatal(w.Code, w.Body.String())
	}
	f.token = &oidc.IDToken{Subject: subject, Nonce: nonce, IssuedAt: time.Now()}
	page, grant := f.callback(url.Values{"state": {state}, "id_token": {"header.payload.signature"}, "code": {"unused"}})
	if grant == "" {
		f.t.Fatal(page.Code, page.Body.String())
	}
	body := page.Body.String()
	if strings.Contains(body, state) || strings.Contains(body, "header.payload.signature") || strings.Contains(body, nonce) {
		f.t.Fatal("landing page leaks the challenge id, nonce or id_token")
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none'; style-src 'unsafe-inline'") {
		f.t.Fatal("csp", csp)
	}
	if !strings.Contains(body, `href="`+app+`://auth/apple?grant=`) {
		f.t.Fatal("landing page does not return to the app", body)
	}
	return grant
}

func TestAppleWebSignIn(t *testing.T) {
	f := newAppleWebFixture(t)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/auth/providers", nil))
	if !strings.Contains(w.Body.String(), `"apple_web":true`) {
		t.Fatal(w.Body.String())
	}
	t.Run("app outside the whitelist", func(t *testing.T) {
		if _, _, w := f.begin("com.attacker.app", "login", "", newVerifier()); w.Code != 400 {
			t.Fatal(w.Code)
		}
	})
	t.Run("bad challenge", func(t *testing.T) {
		w := f.json("/v1/auth/apple/web", "", map[string]string{"code_challenge": "short", "code_challenge_method": "S256", "app": "app.msime.android", "purpose": "login"})
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
		w = f.json("/v1/auth/apple/web", "", map[string]string{"code_challenge": pkceChallenge(newVerifier()), "code_challenge_method": "plain", "app": "app.msime.android", "purpose": "login"})
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	})
	t.Run("challenges endpoint refuses apple_web", func(t *testing.T) {
		if w := f.json("/v1/auth/challenges", "", map[string]string{"provider": "apple_web"}); w.Code != 400 {
			t.Fatal(w.Code)
		}
	})
	t.Run("legacy login refuses apple_web challenges", func(t *testing.T) {
		state, _, _ := f.begin("app.msime.android", "login", "", newVerifier())
		if w := f.json("/v1/auth/login", "", map[string]string{"challenge_id": state, "credential": "id-token"}); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	})
	t.Run("grant without or with a wrong verifier", func(t *testing.T) {
		verifier := newVerifier()
		grant := f.granted("app.msime.android.wubi", "login", "", verifier, "apple-subject-1")
		if w := f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant}); w.Code != 400 {
			t.Fatal("missing verifier", w.Code)
		}
		grant = f.granted("app.msime.android.wubi", "login", "", verifier, "apple-subject-1")
		if w := f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": newVerifier()}); w.Code != 400 {
			t.Fatal("wrong verifier", w.Code)
		}
		// 一次失败的兑换也会作废授权码。
		if w := f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": verifier}); w.Code != 400 {
			t.Fatal("grant survived a failed redemption", w.Code)
		}
	})
	t.Run("login, replay and expiry", func(t *testing.T) {
		verifier := newVerifier()
		grant := f.granted("app.msime.android", "login", "", verifier, "apple-subject-2")
		r := httptest.NewRequest("POST", "/v1/auth/apple/web/login", strings.NewReader(`{"grant":"`+grant+`","code_verifier":"`+verifier+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("User-Agent", "msime-android/1.2.3 (Pixel 8; Android 15; edition=full)")
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "access_token") {
			t.Fatal(w.Code, w.Body.String())
		}
		var agent string
		f.s.pool.QueryRow(context.Background(), "SELECT s.user_agent FROM auth_sessions s JOIN auth_identities i ON i.user_id=s.user_id WHERE i.provider='apple' AND i.subject='apple-subject-2'").Scan(&agent)
		if !strings.Contains(agent, "Pixel 8") {
			t.Fatal("session user agent not recorded", agent)
		}
		if w := f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": verifier}); w.Code != 400 {
			t.Fatal("replay", w.Code)
		}
		grant = f.granted("app.msime.android", "login", "", verifier, "apple-subject-2")
		f.s.pool.Exec(context.Background(), "UPDATE auth_challenges SET granted_at=now()-interval '121 seconds' WHERE provider='apple_web' AND grant_hash<>''")
		if w := f.json("/v1/auth/apple/web/login", "", map[string]string{"grant": grant, "code_verifier": verifier}); w.Code != 400 {
			t.Fatal("expired grant", w.Code)
		}
	})
	t.Run("link requires the same recent session", func(t *testing.T) {
		owner := complete(t, f.s, Identity{"email", "owner@example.test"})
		other := complete(t, f.s, Identity{"email", "other@example.test"})
		verifier := newVerifier()
		grant := f.granted("app.msime.android", "link", owner.AccessToken, verifier, "apple-subject-3")
		if w := f.json("/v1/auth/apple/web/login", other.AccessToken, map[string]string{"grant": grant, "code_verifier": verifier}); w.Code != 400 {
			t.Fatal("other session", w.Code)
		}
		grant = f.granted("app.msime.android", "link", owner.AccessToken, verifier, "apple-subject-3")
		if w := f.json("/v1/auth/apple/web/login", owner.AccessToken, map[string]string{"grant": grant, "code_verifier": verifier}); w.Code != 200 {
			t.Fatal("link", w.Code, w.Body.String())
		}
		var uid string
		f.s.pool.QueryRow(context.Background(), "SELECT user_id FROM auth_identities WHERE provider='apple' AND subject='apple-subject-3'").Scan(&uid)
		if uid != owner.User.ID {
			t.Fatal("identity not linked to the session user")
		}
		if _, _, w := f.begin("app.msime.android", "link", "", verifier); w.Code != 401 {
			t.Fatal("link without session", w.Code)
		}
	})
	t.Run("callback failures", func(t *testing.T) {
		w, grant := f.callback(url.Values{"state": {strings.Repeat("0", 64)}, "id_token": {"x"}})
		if grant != "" || strings.Contains(w.Body.String(), "href=") {
			t.Fatal("unknown state produced a link")
		}
		state, _, _ := f.begin("app.msime.android.japanese", "login", "", newVerifier())
		f.token = &oidc.IDToken{Subject: "s", Nonce: "wrong", IssuedAt: time.Now()}
		w, grant = f.callback(url.Values{"state": {state}, "id_token": {"x"}})
		if grant != "" || !strings.Contains(w.Body.String(), "app.msime.android.japanese://auth/apple?error=invalid_token") {
			t.Fatal(w.Body.String())
		}
		state, _, _ = f.begin("app.msime.android", "login", "", newVerifier())
		w, _ = f.callback(url.Values{"state": {state}, "error": {"user_cancelled_authorize"}, "user": {`<script>alert(1)</script>`}})
		if !strings.Contains(w.Body.String(), "error=cancelled") || strings.Contains(w.Body.String(), "alert(1)") {
			t.Fatal(w.Body.String())
		}
	})
	t.Run("disabled without the services id", func(t *testing.T) {
		f.a.config.Apple.ClientIDs = []string{"app.msime.ios"}
		defer func() { f.a.config.Apple.ClientIDs = []string{"app.msime.ios", AppleServicesID} }()
		if _, _, w := f.begin("app.msime.android", "login", "", newVerifier()); w.Code != 503 {
			t.Fatal(w.Code)
		}
	})
}
