package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCloud answers the sign-in, refresh and a few other routes the way the real service does, with synthetic tokens.
type fakeCloud struct {
	mu        sync.Mutex
	access    string
	refresh   string
	issued    int
	refreshes int
	seen      []string
	// googleURL replaces the authorization URL the Google challenge answers with.
	googleURL string
	// adminState replaces the state the admin sign-in answers with.
	adminState string
	// adminSessions are the admin sessions handed out and not ended.
	adminSessions map[string]bool
	// limited counts down the 429 answers left to give.
	limited int
}

func (f *fakeCloud) issue() map[string]any {
	f.issued++
	f.access, f.refresh = "access-"+string(rune('0'+f.issued)), "refresh-"+string(rune('0'+f.issued))
	return map[string]any{"access_token": f.access, "refresh_token": f.refresh, "token_type": "Bearer", "expires_in": 900, "user": map[string]string{"id": "u1", "display_name": "合成用户"}}
}

func (f *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
	reply := func(status int, value any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(value)
	}
	var body map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /v1/auth/challenges":
		if body["provider"] == "google" {
			target, _ := body["target"].(string)
			authorization := f.googleURL
			if authorization == "" {
				authorization = "https://accounts.google.com/o/oauth2/auth?" + url.Values{"redirect_uri": {target}, "state": {"s-123"}, "client_id": {"synthetic"}}.Encode()
			}
			// The API answers a new challenge with 201 Created.
			reply(201, map[string]any{"challenge_id": "g1", "expires_in": 600, "nonce": "n", "authorization_url": authorization})
			return
		}
		reply(201, map[string]any{"challenge_id": "c1", "expires_in": 300})
	case "POST /v1/auth/login":
		if !(body["challenge_id"] == "c1" && body["credential"] == "123456") && !(body["challenge_id"] == "g1" && body["credential"] == "google-code") {
			reply(401, map[string]any{"error": map[string]string{"code": "invalid_credential"}})
			return
		}
		reply(200, f.issue())
	case "POST /v1/auth/refresh":
		f.refreshes++
		if body["refresh_token"] != f.refresh {
			// A replayed refresh token revokes the session.
			f.access, f.refresh = "", ""
			reply(401, map[string]any{"error": map[string]string{"code": "unauthorized"}})
			return
		}
		reply(200, f.issue())
	case "GET /v1/users/me", "POST /v1/auth/logout":
		if f.access == "" || r.Header.Get("Authorization") != "Bearer "+f.access {
			reply(401, map[string]any{"error": map[string]string{"code": "unauthorized"}})
			return
		}
		reply(200, map[string]any{"id": "u1"})
	case "GET /v1/skins/source":
		w.Header().Set("Content-Type", "application/zip")
		w.Write([]byte("PK\x03\x04synthetic"))
	case "POST /v1/community/plugins":
		file, header, err := r.FormFile("file")
		if err != nil {
			reply(400, map[string]any{"error": map[string]string{"code": "missing_file"}})
			return
		}
		data, _ := io.ReadAll(file)
		reply(201, map[string]any{"name": header.Filename, "size": len(data), "kind": r.FormValue("kind")})
	case "POST /api/auth/cli/start":
		target, _ := body["redirect_uri"].(string)
		state := "s-admin"
		authorization := "https://accounts.google.com/o/oauth2/auth?" + url.Values{"redirect_uri": {target}, "state": {state}, "client_id": {"desktop"}}.Encode()
		if f.adminState != "" {
			state = f.adminState
		}
		reply(200, map[string]any{"state": state, "expires_in": 600, "authorization_url": authorization})
	case "POST /api/auth/cli/finish":
		if body["state"] != "s-admin" || body["code"] != "admin-code" || !strings.HasPrefix(body["redirect_uri"].(string), "http://127.0.0.1:") {
			reply(401, map[string]any{"error": map[string]string{"code": "unauthorized"}})
			return
		}
		if f.adminSessions == nil {
			f.adminSessions = map[string]bool{}
		}
		f.adminSessions["admin-session"] = true
		reply(200, map[string]any{"token": "admin-session", "token_type": "Bearer", "expires_in": 28800, "email": "admin@example.test"})
	case "POST /api/auth/logout":
		delete(f.adminSessions, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		reply(200, map[string]bool{"ok": true})
	case "GET /api/overview", "GET /api/me":
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "admin-key" && !f.adminSessions[token] {
			reply(401, map[string]any{"error": map[string]string{"code": "unauthorized"}})
			return
		}
		reply(200, map[string]any{"users": 3, "range_days": r.URL.Query().Get("days"), "email": "admin@example.test"})
	case "GET /v1/models":
		if f.limited > 0 {
			f.limited--
			w.Header().Set("Retry-After", "1")
			reply(429, map[string]any{"error": map[string]string{"code": "rate_limit_exceeded"}})
			return
		}
		reply(200, map[string]any{"models": []string{"m"}})
	default:
		reply(404, map[string]any{"error": map[string]string{"code": "not_found"}})
	}
}

type harness struct {
	t     *testing.T
	cloud *fakeCloud
	url   string
	dir   string
	env   map[string]string
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	cloud := &fakeCloud{}
	server := httptest.NewServer(cloud)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	return &harness{t: t, cloud: cloud, url: server.URL, dir: dir, now: time.Now(), env: map[string]string{
		"MSIME_CLOUD_URL":        server.URL,
		"MSIME_ADMIN_URL":        server.URL,
		"MSIME_CLOUD_CONFIG_DIR": dir,
	}}
}

func (h *harness) run(stdin string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	c := cli{env: func(name string) string { return h.env[name] }, stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr, client: http.DefaultClient, now: func() time.Time { return h.now }}
	return c.run(args), stdout.String(), stderr.String()
}

func (h *harness) signIn() {
	h.t.Helper()
	if code, out, _ := h.run("", "login", "start", "--email", "someone@example.com"); code != 0 || !strings.Contains(out, `"challenge_id": "c1"`) {
		h.t.Fatalf("login start: %d %s", code, out)
	}
	code, out, errText := h.run("", "login", "finish", "--challenge", "c1", "--code", "123456")
	if code != 0 {
		h.t.Fatalf("login finish: %d %s %s", code, out, errText)
	}
	if strings.Contains(out, "access-") || strings.Contains(out, "refresh-") {
		h.t.Fatalf("tokens printed: %s", out)
	}
}

func TestSignInKeepsTheSessionPrivatelyAndCallsWithIt(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	info, err := os.Stat(filepath.Join(h.dir, "credentials.json"))
	// Windows has no permission bits; the file is private through the per-user ACL on the profile directory it lives in.
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", info, err)
	}
	if code, out, _ := h.run("", "whoami"); code != 0 || !strings.Contains(out, `"id": "u1"`) {
		t.Fatalf("whoami: %d %s", code, out)
	}
	last := h.cloud.seen[len(h.cloud.seen)-1]
	if last != "GET /v1/users/me auth=Bearer access-1" {
		t.Fatalf("whoami sent %q", last)
	}
	// The sign-in itself is anonymous.
	for _, line := range h.cloud.seen[:2] {
		if !strings.HasSuffix(line, "auth=") {
			t.Fatalf("anonymous route got credentials: %s", line)
		}
	}
}

func TestAnExpiredSessionIsRefreshedOnceBeforeTheCall(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.now = h.now.Add(20 * time.Minute)
	if code, _, errText := h.run("", "call", "GET", "/v1/users/me"); code != 0 {
		t.Fatalf("call: %d %s", code, errText)
	}
	if h.cloud.refreshes != 1 || h.cloud.seen[len(h.cloud.seen)-1] != "GET /v1/users/me auth=Bearer access-2" {
		t.Fatalf("refreshes %d, seen %v", h.cloud.refreshes, h.cloud.seen)
	}
}

func TestARejectedAccessTokenIsRefreshedAndRetried(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	// The server ended the access token early, as after a password change elsewhere.
	h.cloud.access = "elsewhere"
	h.cloud.refresh = "refresh-1"
	if code, _, errText := h.run("", "whoami"); code != 0 {
		t.Fatalf("whoami: %d %s", code, errText)
	}
	if h.cloud.refreshes != 1 {
		t.Fatalf("refreshes %d", h.cloud.refreshes)
	}
}

func TestARefreshAlreadyDoneByAnotherCommandIsNotReplayed(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	c := cli{env: func(name string) string { return h.env[name] }, stdout: io.Discard, stderr: io.Discard, client: http.DefaultClient, now: time.Now}
	stale, _, _ := c.store().get(c.server())
	if _, err := c.refresh(c.server(), stale); err != nil {
		t.Fatal(err)
	}
	// A second command that read the session before the first refreshed it.
	latest, err := c.refresh(c.server(), stale)
	if err != nil || latest.AccessToken != "access-2" || h.cloud.refreshes != 1 {
		t.Fatalf("latest %+v err %v refreshes %d", latest, err, h.cloud.refreshes)
	}
}

func TestAnEndedSessionIsForgotten(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.cloud.access, h.cloud.refresh = "", ""
	code, _, errText := h.run("", "whoami")
	if code != 1 || !strings.Contains(errText, "sign in again") {
		t.Fatalf("whoami: %d %s", code, errText)
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, "credentials.json"))
	if strings.Contains(string(data), "access-") {
		t.Fatalf("session kept: %s", data)
	}
}

func TestLogoutEndsAndForgetsTheSession(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code, _, errText := h.run("", "logout"); code != 0 {
		t.Fatalf("logout: %d %s", code, errText)
	}
	if code, _, _ := h.run("", "logout"); code != 1 {
		t.Fatal("a second logout should find no session")
	}
}

func TestAnErrorResponseIsPrintedAndFails(t *testing.T) {
	h := newHarness(t)
	code, out, errText := h.run("", "call", "GET", "/v1/users/me")
	if code != 1 || !strings.Contains(out, "unauthorized") || !strings.Contains(errText, "not signed in") {
		t.Fatalf("call: %d %s %s", code, out, errText)
	}
}

func TestABinaryResponseNeedsAFile(t *testing.T) {
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "device-token"
	if code, _, errText := h.run("", "call", "GET", "/v1/skins/source"); code != 1 || !strings.Contains(errText, "-o") {
		t.Fatalf("call: %d %s", code, errText)
	}
	target := filepath.Join(t.TempDir(), "source.zip")
	code, out, _ := h.run("", "call", "GET", "/v1/skins/source", "-o", target)
	data, _ := os.ReadFile(target)
	if code != 0 || !strings.Contains(out, `"saved"`) || !bytes.HasPrefix(data, []byte("PK")) {
		t.Fatalf("call -o: %d %s %q", code, out, data)
	}
}

func TestCallRequestBodiesAreBound(t *testing.T) {
	const limit = 16 << 20
	largeJSON := `"` + strings.Repeat("a", limit) + `"`
	c := cli{stdin: strings.NewReader(largeJSON)}
	if _, err := c.parseCall([]string{"POST", "/v1/test", "-"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("超大 stdin 请求未被拒绝：%v", err)
	}

	file := filepath.Join(t.TempDir(), "large.bin")
	if err := os.WriteFile(file, []byte(strings.Repeat("a", limit+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (cli{}).parseCall([]string{"POST", "/v1/test", "-F", "file=@" + file}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("超大文件请求未被拒绝：%v", err)
	}
}

func TestFormFieldsUploadAFile(t *testing.T) {
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "device-token"
	pack := filepath.Join(t.TempDir(), "pack.zip")
	os.WriteFile(pack, []byte("synthetic pack"), 0o600)
	code, out, errText := h.run("", "call", "POST", "/v1/community/plugins", "-F", "file=@"+pack, "-F", "kind=effect")
	if code != 0 || !strings.Contains(out, `"name": "pack.zip"`) || !strings.Contains(out, `"kind": "effect"`) {
		t.Fatalf("upload: %d %s %s", code, out, errText)
	}
	if code, _, _ := h.run("{}", "call", "POST", "/v1/community/plugins", "-", "-F", "kind=effect"); code != 2 {
		t.Fatal("a JSON body and form fields together should be refused")
	}
}

func TestAdminPathsUseTheAdminKey(t *testing.T) {
	h := newHarness(t)
	if code, _, errText := h.run("", "call", "GET", "/api/overview"); code != 1 || !strings.Contains(errText, "MSIME_ADMIN_TOKEN") {
		t.Fatalf("without key: %d %s", code, errText)
	}
	h.env["MSIME_ADMIN_TOKEN"] = "admin-key"
	code, out, errText := h.run("", "call", "GET", "/api/overview", "-q", "days=7")
	if code != 0 || !strings.Contains(out, `"range_days": "7"`) {
		t.Fatalf("with key: %d %s %s", code, out, errText)
	}
}

func TestEscapedAdminPathsUseTheAdminSite(t *testing.T) {
	for _, path := range []string{"/api/me", "/%61pi/me", "/api%2Fme"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t)
			admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/me" || r.Header.Get("Authorization") != "Bearer synthetic-admin" {
					t.Errorf("后台请求错误：path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(admin.Close)
			h.env["MSIME_ADMIN_URL"] = admin.URL
			h.env["MSIME_CLOUD_TOKEN"] = "synthetic-user"
			h.env["MSIME_ADMIN_TOKEN"] = "synthetic-admin"
			if code, out, errText := h.run("", "call", "GET", path); code != 0 || out != "" || errText != "" {
				t.Errorf("编码后台路径调用：code=%d out=%q err=%s", code, out, errText)
			}
			h.cloud.mu.Lock()
			defer h.cloud.mu.Unlock()
			if len(h.cloud.seen) != 0 {
				t.Error("编码的后台路径被发往公共服务")
			}
		})
	}
}

func TestRoutesAndDescribeCoverTheAPIAndTheAdminSite(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run("", "routes")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"/v1/users/me", "/v1/auth/login", "/api/actions"} {
		if !strings.Contains(out, want) {
			t.Fatalf("routes lacks %s", want)
		}
	}
	if _, out, _ = h.run("", "routes", "/api/"); strings.Contains(out, "/v1/") {
		t.Fatal("the filter let /v1 through")
	}
	var op operation
	_, out, _ = h.run("", "describe", "get", "/v1/skins/source")
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.Path != "/v1/skins/source" {
		t.Fatalf("a literal segment should beat {id}: %s", out)
	}
	_, out, _ = h.run("", "describe", "GET", "/v1/users/me/dictionaries/pinyin?q=x")
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.Path != "/v1/users/me/dictionaries/{kind}" || op.Auth != authUser {
		t.Fatalf("concrete path: %s", out)
	}
	_, out, _ = h.run("", "describe", "POST", "/v1/community/plugins/p1/download")
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.Path != "/v1/community/plugins/{id}/download" {
		t.Fatalf("parameter in the middle: %s", out)
	}
	if code, _, _ := h.run("", "describe", "GET", "/v1/nothing"); code != 2 {
		t.Fatal("an unknown operation should be a usage error")
	}
}

func TestBadCommandLinesAreUsageErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"call", "GET"},
		{"call", "GET", "v1/users/me"},
		{"call", "POST", "/v1/auth/logout", "{not json"},
		{"call", "GET", "/v1/users/me", "-q"},
		{"login", "start"},
		{"login", "finish", "--challenge", "c1"},
	} {
		if code, _, _ := h.run("", args...); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestAWrongCodeKeepsNoSession(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run("", "login", "finish", "--challenge", "c1", "--code", "000000")
	if code != 1 || !strings.Contains(out, "invalid_credential") {
		t.Fatalf("finish: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("a failed sign-in wrote credentials")
	}
	if code, out, _ := h.run("", "login", "start", "--phone", "+8613800138000"); code != 0 || !strings.Contains(out, "c1") {
		t.Fatalf("phone start: %d %s", code, out)
	}
}

func TestAnUnlockedFileIsReusableAndALiveLockWaitedFor(t *testing.T) {
	s := store{dir: t.TempDir()}
	lock := filepath.Join(s.dir, "credentials.lock")
	os.WriteFile(lock, nil, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	unlock, err := s.lock()
	if err != nil {
		t.Fatalf("an unlocked file should be reusable: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		unlock()
		close(released)
	}()
	second, err := s.lock()
	if err != nil {
		t.Fatalf("a released lock should be taken: %v", err)
	}
	<-released
	second()
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("锁文件应保留，所有进程才能锁住同一个文件", err)
	}
}

func TestAnUnreadableCredentialsFileIsReported(t *testing.T) {
	h := newHarness(t)
	os.WriteFile(filepath.Join(h.dir, "credentials.json"), []byte("{broken"), 0o600)
	if code, _, errText := h.run("", "whoami"); code != 1 || !strings.Contains(errText, "sign in again") {
		t.Fatalf("whoami: %d %s", code, errText)
	}
	if err := (store{}).put("x", nil); err != errNoConfigDir {
		t.Fatalf("a store without a directory: %v", err)
	}
	if _, err := (store{}).lock(); err != errNoConfigDir {
		t.Fatalf("a store without a directory: %v", err)
	}
}

func TestDefaultsPointAtProduction(t *testing.T) {
	c := cli{env: func(string) string { return "" }}
	if c.server() != defaultServer || c.adminServer() != defaultAdminServer {
		t.Fatal(c.server(), c.adminServer())
	}
	if dir := c.store().dir; !strings.HasSuffix(dir, "msime-cloud") {
		t.Fatalf("store dir %q", dir)
	}
	h := newHarness(t)
	if code, out, _ := h.run("", "help"); code != 0 || !strings.Contains(out, "usage: msime-cloud") {
		t.Fatal("help")
	}
	for _, args := range [][]string{{"routes", "a", "b"}, {"describe", "GET"}, {"whoami", "x"}, {"logout", "now"}, {"login"}, {"login", "again"}} {
		if code, _, _ := h.run("", args...); code != 2 {
			t.Fatalf("%v should be a usage error", args)
		}
	}
}

func TestLogoutAllAndAFailedRefreshAreReported(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code, _, errText := h.run("", "logout", "--all"); code != 0 {
		t.Fatalf("logout --all: %d %s", code, errText)
	}
	h.signIn()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"code":"upstream_error"}}`))
	}))
	defer broken.Close()
	c := cli{env: func(name string) string { return h.env[name] }, stdout: io.Discard, stderr: io.Discard, client: http.DefaultClient, now: time.Now}
	current, _, _ := c.store().get(c.server())
	sessions := c.store()
	sessions.put(broken.URL, &current)
	if _, err := c.refresh(broken.URL, current); err == nil || !strings.Contains(err.Error(), "upstream_error") {
		t.Fatalf("refresh: %v", err)
	}
	if kept, ok, _ := sessions.get(broken.URL); !ok || kept.RefreshToken != current.RefreshToken {
		t.Fatal("a refresh the server failed should keep the session for a retry")
	}
}

// browser acts as the user's browser: it follows the authorization URL straight back to the loopback redirect, after a stray request with another state.
func browser(t *testing.T, query url.Values) func(string) error {
	return func(address string) error {
		parsed, err := url.Parse(address)
		if err != nil {
			return err
		}
		redirect := parsed.Query().Get("redirect_uri")
		go func() {
			stray, err := http.Get(redirect + "?state=other&code=x")
			if err != nil || stray.StatusCode != http.StatusNotFound {
				t.Errorf("a callback with another state should be ignored: %v %v", stray, err)
				return
			}
			stray.Body.Close()
			query.Set("state", parsed.Query().Get("state"))
			response, err := http.Get(redirect + "?" + query.Encode())
			if err != nil {
				t.Errorf("callback: %v", err)
				return
			}
			response.Body.Close()
		}()
		return nil
	}
}

func (h *harness) runWithBrowser(browse func(string) error, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	c := cli{env: func(name string) string { return h.env[name] }, stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, client: http.DefaultClient, now: func() time.Time { return h.now }, browse: browse}
	return c.run(args), stdout.String(), stderr.String()
}

func TestGoogleSignInRelaysTheCodeTheBrowserBringsBack(t *testing.T) {
	h := newHarness(t)
	code, out, errText := h.runWithBrowser(browser(t, url.Values{"code": {"google-code"}}), "login", "google")
	if code != 0 || !strings.Contains(out, `"id": "u1"`) || !strings.Contains(errText, "accounts.google.com") {
		t.Fatalf("login google: %d %s %s", code, out, errText)
	}
	if code, _, _ := h.run("", "whoami"); code != 0 {
		t.Fatal("the Google session was not kept")
	}
	for _, line := range h.cloud.seen {
		if strings.HasPrefix(line, "POST /v1/auth/challenges") && !strings.HasSuffix(line, "auth=") {
			t.Fatalf("the challenge carried credentials: %s", line)
		}
	}
}

func TestGoogleSignInCanBeCancelledOrRefused(t *testing.T) {
	h := newHarness(t)
	code, _, errText := h.runWithBrowser(browser(t, url.Values{"error": {"access_denied"}}), "login", "google")
	if code != 1 || !strings.Contains(errText, "cancelled") {
		t.Fatalf("cancelled: %d %s", code, errText)
	}
	opened := false
	h.cloud.googleURL = "https://evil.example/o/oauth2/auth?state=s"
	code, _, errText = h.runWithBrowser(func(string) error { opened = true; return nil }, "login", "google")
	if code != 1 || opened || !strings.Contains(errText, "not one this command opens") {
		t.Fatalf("foreign address: %d %v %s", code, opened, errText)
	}
	if code, _, _ := h.run("", "login", "google", "--browser", "maybe"); code != 2 {
		t.Fatal("an unknown --browser value should be a usage error")
	}
}

func TestGoogleStateIsCheckedAgainstTheLoopbackTarget(t *testing.T) {
	target := "http://127.0.0.1:50000/callback"
	good := "https://accounts.google.com/o/oauth2/auth?" + url.Values{"redirect_uri": {target}, "state": {"s"}}.Encode()
	if state, err := googleState(good, target); err != nil || state != "s" {
		t.Fatalf("good: %q %v", state, err)
	}
	for _, bad := range []string{
		"http://accounts.google.com/o/oauth2/auth?redirect_uri=" + url.QueryEscape(target) + "&state=s",
		"https://accounts.google.com/o/oauth2/auth?redirect_uri=" + url.QueryEscape("http://127.0.0.1:1/callback") + "&state=s",
		"https://accounts.google.com/o/oauth2/auth?redirect_uri=" + url.QueryEscape(target),
		"https://accounts.google.com/o/oauth2/auth?redirect_uri=" + url.QueryEscape(target) + "&state=a&state=b",
		"https://user@accounts.google.com/o/oauth2/auth?redirect_uri=" + url.QueryEscape(target) + "&state=s",
		good + "#fragment",
	} {
		if _, err := googleState(bad, target); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestTheVersionIsTheBackendRelease(t *testing.T) {
	h := newHarness(t)
	if code, out, _ := h.run("", "version"); code != 0 || !strings.HasPrefix(out, "msime-cloud ") || strings.TrimSpace(out) == "msime-cloud" {
		t.Fatalf("version: %d %q", code, out)
	}
}

func TestTheLoopbackGivesUpWithoutACodeOrInTime(t *testing.T) {
	listen := func() (net.Listener, string) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return listener, "http://" + listener.Addr().String() + "/callback"
	}
	listener, address := listen()
	go func() {
		if response, err := http.Get(address + "?state=s"); err == nil {
			response.Body.Close()
		}
	}()
	if _, err := receiveGoogleCode(listener, "s", 5*time.Second); err == nil || !strings.Contains(err.Error(), "without a Google authorization code") {
		t.Fatalf("no code: %v", err)
	}
	listener.Close()
	listener, _ = listen()
	defer listener.Close()
	if _, err := receiveGoogleCode(listener, "s", 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestAdminSignInKeepsASessionForTheAdminSite(t *testing.T) {
	h := newHarness(t)
	if code, _, errText := h.run("", "call", "GET", "/api/overview"); code != 1 || !strings.Contains(errText, "login admin") {
		t.Fatalf("before sign-in: %d %s", code, errText)
	}
	code, out, errText := h.runWithBrowser(browser(t, url.Values{"code": {"admin-code"}}), "login", "admin")
	if code != 0 || !strings.Contains(out, "admin@example.test") || strings.Contains(out, "admin-session") {
		t.Fatalf("login admin: %d %s %s", code, out, errText)
	}
	if code, out, _ := h.run("", "call", "GET", "/api/overview", "-q", "days=7"); code != 0 || !strings.Contains(out, `"range_days": "7"`) {
		t.Fatalf("with the session: %d %s", code, out)
	}
	if code, out, _ := h.run("", "whoami", "--admin"); code != 0 || !strings.Contains(out, "admin@example.test") {
		t.Fatalf("whoami --admin: %d %s", code, out)
	}
	// The user session for the same host is a separate entry.
	if code, _, _ := h.run("", "whoami"); code != 1 {
		t.Fatal("the admin session was used as a user session")
	}
	if code, _, errText := h.run("", "logout", "--admin"); code != 0 || len(h.cloud.adminSessions) != 0 {
		t.Fatalf("logout --admin: %d %s", code, errText)
	}
	if code, _, _ := h.run("", "logout", "--admin"); code != 1 {
		t.Fatal("a second admin logout should find no session")
	}
}

func TestAnEndedOrExpiredAdminSessionIsForgotten(t *testing.T) {
	h := newHarness(t)
	if code, _, errText := h.runWithBrowser(browser(t, url.Values{"code": {"admin-code"}}), "login", "admin"); code != 0 {
		t.Fatal(errText)
	}
	h.cloud.adminSessions = nil
	if code, _, errText := h.run("", "call", "GET", "/api/overview"); code != 1 || !strings.Contains(errText, "has ended") {
		t.Fatalf("ended: %d %s", code, errText)
	}
	if code, _, errText := h.run("", "call", "GET", "/api/overview"); code != 1 || !strings.Contains(errText, "not signed in") {
		t.Fatalf("after ending: %d %s", code, errText)
	}
	if code, _, errText := h.runWithBrowser(browser(t, url.Values{"code": {"admin-code"}}), "login", "admin"); code != 0 {
		t.Fatal(errText)
	}
	h.now = h.now.Add(9 * time.Hour)
	if code, _, errText := h.run("", "call", "GET", "/api/overview"); code != 1 || !strings.Contains(errText, "not signed in") {
		t.Fatalf("expired: %d %s", code, errText)
	}
}

func TestAdminSignInRefusesAStateTheAddressDoesNotCarry(t *testing.T) {
	h := newHarness(t)
	h.cloud.adminState = "another"
	opened := false
	code, _, errText := h.runWithBrowser(func(string) error { opened = true; return nil }, "login", "admin")
	if code != 1 || opened || !strings.Contains(errText, "state") {
		t.Fatalf("login admin: %d %v %s", code, opened, errText)
	}
}

func TestARateLimitIsWaitedOutOnce(t *testing.T) {
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "device-token"
	h.cloud.limited = 1
	code, out, errText := h.run("", "call", "GET", "/v1/models")
	if code != 0 || !strings.Contains(out, `"m"`) || !strings.Contains(errText, "retrying in 1s") {
		t.Fatalf("one 429: %d %s %s", code, out, errText)
	}
	h.cloud.limited = 2
	if code, out, _ := h.run("", "call", "GET", "/v1/models"); code != 1 || !strings.Contains(out, "rate_limit_exceeded") {
		t.Fatalf("two 429s: %d %s", code, out)
	}
}

func TestTheLiveAudioWebSocketIsRefused(t *testing.T) {
	h := newHarness(t)
	h.env["MSIME_CLOUD_TOKEN"] = "device-token"
	for _, path := range []string{"/v1/audio/stream?model=x", "/v1/audio/%73tream", "/v1/audio/stream#ignored"} {
		if code, _, errText := h.run("", "call", "GET", path); code != 1 || !strings.Contains(errText, "WebSocket") {
			t.Errorf("实时语音路径=%s：code=%d err=%s", path, code, errText)
		}
	}
}

func TestEncodedSeparatorDoesNotSelectWebSocket(t *testing.T) {
	h := newHarness(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	h.env["MSIME_CLOUD_URL"] = server.URL
	if code, _, errText := h.run("", "call", "GET", "/v1%2Faudio/stream"); code != 0 || strings.Contains(errText, "WebSocket") {
		t.Errorf("编码斜杠被当成路径分隔符：code=%d err=%s", code, errText)
	}
}

func TestCallQueryPrecedesURLFragment(t *testing.T) {
	for _, tc := range []struct{ path, wantURI string }{
		{"/v1/notices#ignored", "/v1/notices?limit=2"},
		{"/v1/notices?limit=1#ignored", "/v1/notices?limit=1&limit=2"},
		{"/v1/notices#ignored?x=1", "/v1/notices?limit=2"},
		{"/v1/items/has%23hash", "/v1/items/has%23hash?limit=2"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != tc.wantURI {
					t.Errorf("查询参数没有正确发送：uri=%s，期望 %s", r.URL.RequestURI(), tc.wantURI)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(server.Close)
			c := cli{client: server.Client()}
			response, err := c.doOnce(server.URL, request{method: http.MethodGet, path: tc.path, query: url.Values{"limit": {"2"}}}, "")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		})
	}
}

func TestAdminRoutesComeFromTheAdminSiteTables(t *testing.T) {
	h := newHarness(t)
	_, out, _ := h.run("", "routes", "/api/")
	for _, want := range []string{"POST   /api/actions", "GET    /api/users ", "*      /api/crash-groups/{id}/issue", "*      /api/dict-prs/{rest...}", "GET    /api/system", "POST   /api/admins"} {
		if !strings.Contains(out, want) {
			t.Errorf("routes lacks %q", want)
		}
	}
	var op operation
	_, out, _ = h.run("", "describe", "POST", "/api/actions")
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.Auth != authAdmin || len(op.Guide) == 0 || !strings.Contains(strings.Join(op.Guide, "\n"), "ids") {
		t.Fatalf("describe POST /api/actions: %s", out)
	}
	_, out, _ = h.run("", "describe", "POST", "/api/crash-groups/abc/issue")
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.Path != "/api/crash-groups/{id}/issue" {
		t.Fatalf("any-method route: %s", out)
	}
	if !mentions("GET /api/users?page=2", "/api/users") || !mentions("`/api/users/{id}`", "/api/users") || mentions("/api/users-archive", "/api/users") {
		t.Fatal("mentions matches longer paths or misses the path itself")
	}
}
