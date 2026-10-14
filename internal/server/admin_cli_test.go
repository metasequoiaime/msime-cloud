package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"golang.org/x/oauth2"
)

func TestAdminCommandLineSignIn(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "http://127.0.0.1:50123/callback"
	store := &adminMemoryStore{flows: map[string]account.AdminLoginFlow{}, sessions: map[string]account.AdminIdentity{}}
	var claims map[string]any
	var challenge string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		// The exchange goes through the desktop client, with the loopback redirect the code was issued for.
		if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("client_id") != "desktop-client" || r.Form.Get("client_secret") != "desktop-secret" || r.Form.Get("redirect_uri") != redirect {
			t.Error("invalid code exchange", r.Form)
		}
		payload, _ := json.Marshal(claims)
		signed, _ := signer.Sign(payload)
		raw, _ := signed.CompactSerialize()
		respond(w, 200, map[string]any{"access_token": "unused", "token_type": "Bearer", "id_token": raw})
	}))
	defer provider.Close()
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", Google: AdminGoogleConfig{ClientID: "google-client", RedirectURI: "https://admin.msime.app" + adminCallbackPath, AllowedEmails: []string{"admin@example.test"}}}
	s.adminStore = store
	endpoint := oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: provider.URL, AuthStyle: oauth2.AuthStyleInParams}
	keys := adminTestKeys{&key.PublicKey}
	s.adminGoogle = &adminGoogleAuth{oauth: oauth2.Config{ClientID: "google-client", ClientSecret: "secret", RedirectURL: s.config.Admin.Google.RedirectURI, Scopes: []string{"openid", "email"}, Endpoint: endpoint}, verifier: oidc.NewVerifier("https://accounts.google.com", keys, &oidc.Config{ClientID: "google-client", SupportedSigningAlgs: []string{"RS256"}})}
	call := func(method, path, body, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://admin.msime.app"+path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		r.Header.Set("User-Agent", "msime-cloud/test")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// Without the desktop client there is no command-line sign-in.
	if w := call("POST", "/api/auth/cli/start", `{"redirect_uri":"`+redirect+`"}`, ""); w.Code != 404 || !strings.Contains(w.Body.String(), "cli_login_disabled") {
		t.Fatal(w.Code, w.Body.String())
	}
	s.adminCLI = &adminGoogleAuth{oauth: oauth2.Config{ClientID: "desktop-client", ClientSecret: "desktop-secret", Scopes: adminGoogleScopes, Endpoint: endpoint}, verifier: oidc.NewVerifier("https://accounts.google.com", keys, &oidc.Config{ClientID: "desktop-client", SupportedSigningAlgs: []string{"RS256"}})}
	if w := call("GET", "/api/auth/session", "", ""); !strings.Contains(w.Body.String(), `"cli_enabled":true`) {
		t.Fatal(w.Body.String())
	}
	for _, bad := range []string{
		`{"redirect_uri":"https://evil.test/callback"}`,
		`{"redirect_uri":"http://127.0.0.1:80/callback"}`,
		`{"redirect_uri":"http://127.0.0.1:50123/other"}`,
		`{"redirect_uri":"` + redirect + `","extra":1}`,
		`not json`,
		`{"redirect_uri":"` + redirect + `"} {}`,
		`{"redirect_uri":"` + redirect + `"} null`,
		`{"redirect_uri":"` + redirect + `"} trailing`,
		`{"redirect_uri":"` + redirect + `"}` + strings.Repeat(" ", 16<<10),
	} {
		if w := call("POST", "/api/auth/cli/start", bad, ""); w.Code != 400 {
			t.Errorf("无效登录请求被接受：长度=%d 状态=%d 响应=%s", len(bad), w.Code, w.Body.String())
		}
	}
	if len(store.flows) != 0 {
		t.Error("无效登录请求创建了登录状态")
	}
	resetLimits := func() {
		s.mu.Lock()
		s.buckets = map[string]bucket{}
		s.mu.Unlock()
	}
	start := func() (string, string) {
		t.Helper()
		resetLimits()
		w := call("POST", "/api/auth/cli/start", " \t"+`{"redirect_uri":"`+redirect+`"}`+"\r\n", "")
		var started struct {
			State            string `json:"state"`
			AuthorizationURL string `json:"authorization_url"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &started) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		u, _ := url.Parse(started.AuthorizationURL)
		params := u.Query()
		challenge = params.Get("code_challenge")
		if u.Host != "accounts.google.com" || params.Get("client_id") != "desktop-client" || params.Get("redirect_uri") != redirect || params.Get("state") != started.State || params.Get("code_challenge_method") != "S256" || params.Get("scope") != "openid email profile" || params.Get("nonce") == "" {
			t.Fatal(started.AuthorizationURL)
		}
		claims = map[string]any{"iss": "https://accounts.google.com", "aud": "desktop-client", "sub": "google-admin", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": params.Get("nonce"), "email": "Admin@example.test", "email_verified": true, "name": "管理员"}
		return started.State, `{"state":"` + started.State + `","code":"code","redirect_uri":"` + redirect + `"}`
	}
	for _, tc := range []struct {
		field string
		value any
	}{{"aud", "google-client"}, {"nonce", "wrong"}, {"email", "other@example.test"}, {"email_verified", false}, {"iat", time.Now().Add(time.Hour).Unix()}, {"exp", time.Now().Add(-time.Hour).Unix()}} {
		_, body := start()
		claims[tc.field] = tc.value
		if w := call("POST", "/api/auth/cli/finish", body, ""); w.Code != 401 || len(store.sessions) != 0 {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	if w := call("POST", "/api/auth/cli/finish", `{"state":"`+strings.Repeat("a", 64)+`","code":"code","redirect_uri":"`+redirect+`"}`, ""); w.Code != 401 {
		t.Fatal("an unknown state was accepted", w.Code)
	}
	_, body := start()
	// 无效的尾随内容必须在消费 state、调用令牌交换或创建会话之前拒绝。
	for _, suffix := range []string{" {}", " null", " trailing", strings.Repeat(" ", 16<<10)} {
		w := call("POST", "/api/auth/cli/finish", body+suffix, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Errorf("无效登录完成请求被接受：长度=%d 状态=%d 响应=%s", len(suffix), w.Code, w.Body.String())
		}
		if len(store.sessions) != 0 {
			t.Error("无效登录完成请求创建了管理员会话")
		}
	}
	w := call("POST", "/api/auth/cli/finish", body, "")
	var finished struct {
		Token     string `json:"token"`
		Email     string `json:"email"`
		ExpiresIn int    `json:"expires_in"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &finished) != nil || len(finished.Token) != 64 || finished.Email != "admin@example.test" || finished.ExpiresIn != 28800 {
		t.Fatal(w.Code, w.Body.String())
	}
	if kept := store.sessions[finished.Token]; kept.Name != "管理员" || kept.UserAgent != "msime-cloud/test" {
		t.Fatalf("the session should record the name and client like the web sign-in: %+v", kept)
	}
	if w = call("POST", "/api/auth/cli/finish", body, ""); w.Code != 401 || len(store.sessions) != 1 {
		t.Fatal("replay accepted", w.Code)
	}
	// The session works as a bearer token, needs no Origin for a change, and ends with logout.
	if w = call("GET", "/api/auth/session", "", finished.Token); !strings.Contains(w.Body.String(), `"authenticated":true`) || !strings.Contains(w.Body.String(), "admin@example.test") {
		t.Fatal(w.Body.String())
	}
	if w = call("GET", "/api/system", "", finished.Token); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("GET", "/api/system", "", strings.Repeat("b", 64)); w.Code != 401 {
		t.Fatal("an unknown bearer session was accepted", w.Code)
	}
	if w = call("POST", "/api/auth/logout", "", finished.Token); w.Code != 200 || len(store.sessions) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("GET", "/api/system", "", finished.Token); w.Code != 401 {
		t.Fatal("a logged-out bearer session was accepted", w.Code)
	}
	if w = call("GET", "/api/auth/cli/start", "", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
	// Starting a sign-in shares the web sign-in's limit of ten a minute per address.
	resetLimits()
	for i := 0; i < 10; i++ {
		call("POST", "/api/auth/cli/start", `{"redirect_uri":"`+redirect+`"}`, "")
	}
	if w = call("POST", "/api/auth/cli/start", `{"redirect_uri":"`+redirect+`"}`, ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("starts are not limited", w.Code)
	}
	// 在代理之后，额度属于 `client_ip_header` 报告的地址，而不是所有请求都经过的代理。
	s.config.ClientIPHeader = "CF-Connecting-IP"
	behindProxy := func(client string) int {
		r := httptest.NewRequest("POST", "https://admin.msime.app/api/auth/cli/start", strings.NewReader(`{"redirect_uri":"`+redirect+`"}`))
		r.Header.Set("CF-Connecting-IP", client)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	if code := behindProxy("198.51.100.50"); code != 200 {
		t.Fatal("another administrator behind the same proxy was limited", code)
	}
}

func TestReadAdminJSONSizeBoundary(t *testing.T) {
	const body = `{"redirect_uri":"http://127.0.0.1:50123/callback"}`
	for _, size := range []int{16 << 10, (16 << 10) + 1} {
		r := httptest.NewRequest("POST", "/api/auth/cli/start", strings.NewReader(body+strings.Repeat(" ", size-len(body))))
		w := httptest.NewRecorder()
		var value struct {
			RedirectURI string `json:"redirect_uri"`
		}
		accepted := readAdminJSON(w, r, &value)
		if size == 16<<10 {
			if !accepted || value.RedirectURI != "http://127.0.0.1:50123/callback" {
				t.Fatal("上限内带尾随空白的合法请求被拒绝", w.Code, w.Body.String())
			}
		} else if accepted || w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Fatal("含尾随空白的请求绕过大小上限", accepted, w.Code, w.Body.String())
		}
	}
}

func TestReadAdminJSONRejectsFailureAfterObject(t *testing.T) {
	const message = "合成请求体读取故障"
	r := httptest.NewRequest("POST", "/api/auth/cli/start", io.MultiReader(strings.NewReader(`{"redirect_uri":"http://127.0.0.1:50123/callback"}`), iotest.ErrReader(errors.New(message))))
	w := httptest.NewRecorder()
	var value struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if readAdminJSON(w, r, &value) || w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request") || strings.Contains(w.Body.String(), message) {
		t.Fatal("对象之后的读取故障被忽略或泄露", w.Code, w.Body.String())
	}
}
