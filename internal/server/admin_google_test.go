package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/oauth2"
)

type adminMemoryStore struct {
	allowed  map[string]bool
	flows    map[string]account.AdminLoginFlow
	sessions map[string]account.AdminIdentity
	// roles maps a member to its role and permissions; a member without an entry is a maintainer with every permission.
	roles map[string]adminMemoryRole
	// tokens are personal access tokens.
	tokens map[string]account.AdminIdentity
}

type adminMemoryRole struct {
	role  string
	perms []string
}

func (m *adminMemoryStore) SaveAdminFlow(_ context.Context, k string, v account.AdminLoginFlow) error {
	m.flows[k] = v
	return nil
}
func (m *adminMemoryStore) ConsumeAdminFlow(_ context.Context, k string) (account.AdminLoginFlow, error) {
	v, ok := m.flows[k]
	delete(m.flows, k)
	if !ok {
		return v, account.ErrInvalid
	}
	return v, nil
}
func (m *adminMemoryStore) CreateAdminSession(_ context.Context, v account.AdminIdentity) (string, error) {
	k := adminRandom()
	m.sessions[k] = v
	return k, nil
}
func (m *adminMemoryStore) AdminSession(_ context.Context, k string) (account.AdminIdentity, error) {
	v, ok := m.sessions[k]
	if !ok {
		return v, account.ErrInvalid
	}
	return v, nil
}
func (m *adminMemoryStore) DeleteAdminSession(_ context.Context, tokens ...string) error {
	for _, token := range tokens {
		delete(m.sessions, token)
	}
	return nil
}

func (m *adminMemoryStore) AdminEmailAllowed(_ context.Context, email string) (bool, error) {
	return m.allowed[email], nil
}
func (m *adminMemoryStore) AdminMemberRole(_ context.Context, email string) (string, []string, error) {
	if !m.allowed[email] {
		return "", nil, account.ErrInvalid
	}
	if role, ok := m.roles[email]; ok {
		return role.role, role.perms, nil
	}
	return account.RoleMaintainer, account.AllAdminPermissions(), nil
}
func (m *adminMemoryStore) AdminTokenIdentity(_ context.Context, token string) (account.AdminIdentity, error) {
	v, ok := m.tokens[token]
	if !ok {
		return v, account.ErrInvalid
	}
	return v, nil
}

type adminTestKeys struct{ public *rsa.PublicKey }

func (k adminTestKeys) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	token, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, err
	}
	return token.Verify(k.public)
}

func TestAdminGoogleFlow(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &adminMemoryStore{flows: map[string]account.AdminLoginFlow{}, sessions: map[string]account.AdminIdentity{}}
	var claims map[string]any
	var challenge string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("client_secret") != "secret" || r.Form.Get("redirect_uri") != "https://admin.msime.app"+adminCallbackPath {
			t.Error("invalid code exchange")
		}
		payload, _ := json.Marshal(claims)
		signed, _ := signer.Sign(payload)
		raw, _ := signed.CompactSerialize()
		if claims["bad_signature"] == true {
			parts := strings.Split(raw, ".")
			parts[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
			raw = strings.Join(parts, ".")
		}
		respond(w, 200, map[string]any{"access_token": "unused", "token_type": "Bearer", "id_token": raw})
	}))
	defer provider.Close()
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", Google: AdminGoogleConfig{ClientID: "google-client", RedirectURI: "https://admin.msime.app" + adminCallbackPath, AllowedEmails: []string{"admin@example.test"}}}
	s.adminStore = store
	s.adminGoogle = &adminGoogleAuth{oauth: oauth2.Config{ClientID: "google-client", ClientSecret: "secret", RedirectURL: s.config.Admin.Google.RedirectURI, Scopes: adminGoogleScopes, Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: provider.URL, AuthStyle: oauth2.AuthStyleInParams}}, verifier: oidc.NewVerifier("https://accounts.google.com", adminTestKeys{&key.PublicKey}, &oidc.Config{ClientID: "google-client", SupportedSigningAlgs: []string{"RS256"}})}
	call := func(method, path, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://admin.msime.app"+path, nil)
		r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	start := func() (*http.Cookie, string) {
		t.Helper()
		w := call("GET", "/api/auth/google/start", "", nil)
		if w.Code != 302 {
			t.Fatal(w.Code, w.Body.String())
		}
		u, _ := url.Parse(w.Header().Get("Location"))
		params := u.Query()
		challenge = params.Get("code_challenge")
		if params.Get("code_challenge_method") != "S256" || params.Get("scope") != "openid email profile" || params.Get("nonce") == "" {
			t.Fatal(params)
		}
		cookie := w.Result().Cookies()[0]
		if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatal(cookie)
		}
		claims = map[string]any{"iss": "https://accounts.google.com", "aud": "google-client", "sub": "google-admin", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": params.Get("nonce"), "email": "admin@example.test", "email_verified": true, "name": "Admin\u0007 Person"}
		return cookie, adminCallbackPath + "?state=" + params.Get("state") + "&code=code"
	}
	for _, tc := range []struct {
		field string
		value any
	}{{"bad_signature", true}, {"iat", time.Now().Add(time.Hour).Unix()}, {"email", "other@example.test"}, {"email_verified", false}, {"nonce", "wrong"}, {"aud", "other-client"}, {"iss", "https://evil.test"}, {"exp", time.Now().Add(-time.Hour).Unix()}} {
		cookie, path := start()
		claims[tc.field] = tc.value
		w := call("GET", path, "", cookie)
		if w.Code != 303 || w.Header().Get("Location") != "/?login_error=google_denied" || len(store.sessions) != 0 {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	cookie, path := start()
	if w := call("GET", path, "", nil); w.Code != 303 || len(store.sessions) != 0 {
		t.Fatal("missing binding cookie accepted")
	}
	// 模拟旧请求成功前浏览器已开始新流程，迟到的成功响应也必须保留新绑定。
	newState := strings.Repeat("b", 64)
	store.flows[newState] = account.AdminLoginFlow{}
	target, _ := url.Parse("https://admin.msime.app")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	issued := httptest.NewRecorder()
	s.adminCookie(issued, newState, true, 600)
	jar.SetCookies(target, issued.Result().Cookies())
	w := call("GET", path, "", cookie)
	jar.SetCookies(target, w.Result().Cookies())
	flowKept := false
	for _, c := range jar.Cookies(target) {
		if c.Name == s.adminCookieName(true) && c.Value == newState {
			flowKept = true
		}
	}
	if !flowKept {
		t.Error("旧登录成功响应清掉了新流程Cookie")
	}
	if _, ok := store.flows[newState]; !ok {
		t.Error("旧登录成功回调消费了新流程")
	}
	if w.Code != 303 || w.Header().Get("Location") != "/" || len(store.sessions) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == s.adminCookieName(false) {
			session = c
		}
	}
	if session == nil || session.MaxAge != 28800 || !session.HttpOnly || !session.Secure {
		t.Fatal(session)
	}
	// The profile name and the browser are handed to the store for the personal page; the store strips control characters.
	if stored := store.sessions[session.Value]; stored.Name != "Admin\u0007 Person" || !strings.Contains(stored.UserAgent, "Safari") {
		t.Fatal(stored)
	}
	if w = call("GET", path, "", cookie); w.Header().Get("Location") != "/?login_error=google_denied" {
		t.Fatal("replay accepted")
	}
	if w = call("GET", "/api/auth/session", "", session); w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":true`) || !strings.Contains(w.Body.String(), "admin@example.test") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, origin := range []string{"", "https://evil.test", "http://admin.msime.app"} {
		if w = call("POST", "/api/auth/logout", origin, session); w.Code != 403 {
			t.Fatal("CSRF accepted", origin, w.Code)
		}
	}
	// A configuration allowlist removal invalidates previously issued sessions.
	s.config.Admin.Google.AllowedEmails = nil
	if w = call("GET", "/api/auth/session", "", session); !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatal(w.Body.String())
	}
	s.config.Admin.Google.AllowedEmails = []string{"admin@example.test"}
	if w = call("POST", "/api/auth/logout", "https://admin.msime.app", session); w.Code != 200 || len(store.sessions) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("GET", "/api/auth/session", "", session); !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatal("logout cookie still accepted")
	}
}

func TestAdminGoogleConfiguration(t *testing.T) {
	t.Setenv("GOOGLE_SECRET_TEST", "test-secret")
	t.Setenv("GOOGLE_EMAILS_TEST", "Admin@example.test")
	c := AdminConfig{Enabled: true, Host: "admin.msime.app", TokenEnv: "UNSET_ADMIN_TEST", Google: AdminGoogleConfig{ClientID: "client", SecretEnv: "GOOGLE_SECRET_TEST", AllowedEmailsEnv: "GOOGLE_EMAILS_TEST"}}
	t.Setenv("UNSET_ADMIN_TEST", "")
	if err := c.validate(true, nil); err != nil || c.Google.RedirectURI != "https://admin.msime.app"+adminCallbackPath || !c.Google.allows("admin@example.test") {
		t.Fatal(c, err)
	}
	c.Google.RedirectURI = "https://evil.test" + adminCallbackPath
	if err := c.validate(true, nil); err == nil {
		t.Fatal("foreign callback allowed")
	}
	c.Google.RedirectURI = "http://admin.msime.app" + adminCallbackPath
	if err := c.validate(true, nil); err == nil {
		t.Fatal("insecure production callback allowed")
	}
	c.Google.RedirectURI = ""
	t.Setenv("GOOGLE_EMAILS_TEST", "")
	if err := c.validate(true, nil); err == nil {
		t.Fatal("empty whitelist accepted")
	}
}

func TestManagedAdminAuthorization(t *testing.T) {
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", Google: AdminGoogleConfig{AllowedEmails: []string{"owner@example.test"}}}
	s.adminGoogle = &adminGoogleAuth{}
	token := strings.Repeat("a", 64)
	store := &adminMemoryStore{sessions: map[string]account.AdminIdentity{token: {Subject: "member", Email: "member@example.test"}}, allowed: map[string]bool{"member@example.test": true}}
	s.adminStore = store
	r := httptest.NewRequest("GET", "https://admin.msime.app/api/admins", nil)
	r.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: token})
	if _, _, err := s.adminIdentity(r); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("member can manage admins", w.Code)
	}
	store.allowed["member@example.test"] = false
	if _, _, err := s.adminIdentity(r); err != account.ErrInvalid {
		t.Fatal("disabled member accepted", err)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("disabled member authorized", w.Code)
	}
}
