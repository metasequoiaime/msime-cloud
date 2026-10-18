package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// fakeCrashGitHub serves the installation token and issue creation, recording each issue it was asked to open.
type fakeCrashGitHub struct {
	mu     sync.Mutex
	issues []map[string]any
	paths  []string
	status int
}

func (f *fakeCrashGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.paths = append(f.paths, key)
	w.Header().Set("Content-Type", "application/json")
	if key == "POST /app/installations/77/access_tokens" {
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_crash", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer ghs_crash" || key != "POST /repos/metasequoiaime/msime-ios/issues" {
		w.WriteHeader(404)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.issues = append(f.issues, body)
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "html_url": "https://github.com/metasequoiaime/msime-ios/issues/7"})
}

func (f *fakeCrashGitHub) calls(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, path := range f.paths {
		if path == key {
			n++
		}
	}
	return n
}

// Without a database or GitHub the endpoint still validates the method, permission and signature before anything else.
func TestAdminCrashIssueBoundaries(t *testing.T) {
	s, _ := adminRBACFixture(t)
	legacy := strings.Repeat("a", 40)
	for _, tc := range []struct {
		method, path, bearer, code string
		status                     int
	}{
		{"PUT", "/api/crash-groups/0123456789abcdef/issue", legacy, "method_not_allowed", 405},
		{"POST", "/api/crash-groups/0123456789abcdef/issue", account.AdminTokenPrefix + "readonly", "permission_denied", 403},
		{"POST", "/api/crash-groups/NOTHEX/issue", legacy, "invalid_id", 400},
		{"GET", "/api/crash-groups/a/b/issue", legacy, "invalid_id", 400},
		{"POST", "/api/crash-groups/0123456789abcdef/issue", legacy, "github_disabled", 404},
		{"GET", "/api/crash-groups/0123456789abcdef/issue", account.AdminTokenPrefix + "readonly", "github_disabled", 404},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, tc.method, tc.path, "", tc.bearer))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatal(tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	// Bodies other than {} are refused.
	r := httptest.NewRequest("POST", "https://admin.msime.app/api/crash-groups/0123456789abcdef/issue", strings.NewReader(`{"title":"x"}`))
	r.Header.Set("Authorization", "Bearer "+legacy)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	// The spike job returns at once without accounts and when its context ends.
	s.crashSpikeJob(context.Background())
}

func TestAdminCrashIssue(t *testing.T) {
	disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("q", 48))
	s, err := New(Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin:   AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN", GitHub: AdminGitHubConfig{Platforms: []AdminPlatformConfig{{ID: "ios", Name: "iOS", Repo: "metasequoiaime/msime-ios", TagPrefix: "ios-v", Label: "ios", Assignee: "houko"}}}},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	ctx := context.Background()
	db, err := pgx.Connect(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	const ios, android = "0123456789abcdef", "fedcba9876543210"
	if _, err = db.Exec(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES($1,'ios','1.0.0','EXC_BAD_ACCESS @octocat'),($2,'android','0.1.0','SIGSEGV')`, ios, android); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,message,stack,signature) VALUES('crash-issue-ios-001','crash','ios','1.0.0','EXC_BAD_ACCESS @octocat','2 MSIME 0x1 Keyboard.layout() + 1
`+"```"+` fence breaker',$1)`, ios); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCrashGitHub{}
	upstream := httptest.NewServer(fake)
	defer upstream.Close()
	s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &githubapp.Cache{}}
	call := func(method, signature string) (*httptest.ResponseRecorder, map[string]any) {
		var r *http.Request
		if method == "POST" {
			r = httptest.NewRequest(method, "https://admin.example.com/api/crash-groups/"+signature+"/issue", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, "https://admin.example.com/api/crash-groups/"+signature+"/issue", nil)
		}
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("q", 48))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w, body
	}
	audits := func(signature string) int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE action='crash_group_issue' AND target=$1 AND actor='legacy-token'`, signature).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The target is known before anyone opens an issue, and is null for a platform without a repository.
	if w, body := call("GET", ios); w.Code != 200 || body["target"].(map[string]any)["repo"] != "metasequoiaime/msime-ios" || body["issue_url"] != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if w, body := call("GET", android); w.Code != 200 || body["target"] != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	t.Run("目标探测不读取事件表", func(t *testing.T) {
		lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		tx, err := db.Begin(lockCtx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(lockCtx, `LOCK TABLE admin_events IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		requestCtx, cancelRequest := context.WithTimeout(ctx, 2*time.Second)
		defer cancelRequest()
		r := httptest.NewRequest("GET", "https://admin.example.com/api/crash-groups/"+ios+"/issue", nil).WithContext(requestCtx)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("q", 48))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "metasequoiaime/msime-ios") {
			t.Fatal("目标探测仍依赖事件表", w.Code, w.Body.String())
		}
	})
	if w, _ := call("GET", "00000000000000aa"); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w, _ := call("POST", android); w.Code != 409 || !strings.Contains(w.Body.String(), "platform_not_configured") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w, _ := call("POST", "00000000000000aa"); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}

	// GitHub 明确拒绝时恢复未处理，不记录创建成功审计。
	for status, code := range map[int]string{422: "github_rejected", 410: "github_rejected"} {
		fake.status = status
		if w, _ := call("POST", ios); w.Code != 502 || !strings.Contains(w.Body.String(), code) {
			t.Fatal(status, w.Code, w.Body.String())
		}
	}
	fake.status = 0
	var status string
	var issueURL *string
	if err = db.QueryRow(ctx, `SELECT status,issue_url FROM admin_crash_groups WHERE signature=$1`, ios).Scan(&status, &issueURL); err != nil || status != "open" || issueURL != nil || audits(ios) != 0 {
		t.Fatal(status, issueURL, err)
	}

	// While another request holds the group's issue lock, a second request is refused without calling GitHub.
	locked, err := s.accounts.LockCrashGroupIssue(ctx, ios)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.accounts.LockCrashGroupIssue(ctx, ios); err != account.ErrCrashIssueBusy {
		t.Fatal(err)
	}
	before := fake.calls("POST /repos/metasequoiaime/msime-ios/issues")
	if w, _ := call("POST", ios); w.Code != 409 || !strings.Contains(w.Body.String(), "issue_in_progress") || fake.calls("POST /repos/metasequoiaime/msime-ios/issues") != before {
		t.Fatal(w.Code, w.Body.String())
	}
	locked.Release()

	w, body := call("POST", ios)
	if w.Code != 200 || body["issue_url"] != "https://github.com/metasequoiaime/msime-ios/issues/7" || body["status"] != "known" || body["number"] != float64(7) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = db.QueryRow(ctx, `SELECT status,issue_url FROM admin_crash_groups WHERE signature=$1`, ios).Scan(&status, &issueURL); err != nil || status != "known" || issueURL == nil || *issueURL != "https://github.com/metasequoiaime/msime-ios/issues/7" || audits(ios) != 1 {
		t.Fatal(status, issueURL, err)
	}
	if len(fake.issues) != 1 {
		t.Fatal(fake.issues)
	}
	issue := fake.issues[0]
	text, _ := issue["body"].(string)
	if issue["title"] != "Crash: EXC_BAD_ACCESS @octocat" || !strings.Contains(text, "Crash group ` 0123456789abcdef `") || !strings.Contains(text, "````text\n2 MSIME 0x1 Keyboard.layout() + 1\n``` fence breaker\n````") || !strings.Contains(text, "- Platform: ` ios `") {
		t.Fatalf("%q %q", issue["title"], text)
	}
	if labels, _ := issue["labels"].([]any); len(labels) != 1 || labels[0] != "ios" {
		t.Fatal(issue["labels"])
	}
	if assignees, _ := issue["assignees"].([]any); len(assignees) != 1 || assignees[0] != "houko" {
		t.Fatal(issue["assignees"])
	}

	// A second request does not open a duplicate.
	if w, body := call("POST", ios); w.Code != 409 || body["issue_url"] != "https://github.com/metasequoiaime/msime-ios/issues/7" {
		t.Fatal(w.Code, w.Body.String())
	}
	if fake.calls("POST /repos/metasequoiaime/msime-ios/issues") != 3 {
		t.Fatal(fake.paths)
	}
	if w, body := call("GET", ios); w.Code != 200 || body["issue_url"] != "https://github.com/metasequoiaime/msime-ios/issues/7" {
		t.Fatal(w.Code, w.Body.String())
	}

	// The spike job runs a check and stops with its context.
	jobCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		s.crashSpikeJob(jobCtx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("crashSpikeJob did not return after its context ended")
	}
}
