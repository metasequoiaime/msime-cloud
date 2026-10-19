package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// fakeIssue is one issue (or pull request) in fakeIssuesGitHub.
type fakeIssue struct {
	id          int64
	number      int
	title, body string
	author      string
	state       string
	stateReason string
	labels      []string
	assignees   []string
	created     time.Time
	pull        bool
	timeline    []map[string]any
}

type fakeIssueComment struct {
	number      int
	login, kind string
	association string
	created     time.Time
}

// fakeIssuesGitHub stands in for the GitHub REST API of the issue repositories: installation tokens, listing, reading, labelling, assigning, closing, reopening and commenting.
type fakeIssuesGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	server   *httptest.Server
	issues   map[string]map[int]*fakeIssue
	comments map[string][]fakeIssueComment
	// status overrides the answer to "METHOD path" with an error status.
	status map[string]int
	// writes lists every non-GET call except token minting, in order.
	writes []string
	bodies map[string]map[string]any
	tokens map[string]int
	nextID int64
	// before 在处理每个请求之前、不持有 mu 时调用，测试用它让某些读取阻塞或记录读取顺序。
	before func(r *http.Request)
}

func newFakeIssuesGitHub(t *testing.T) *fakeIssuesGitHub {
	f := &fakeIssuesGitHub{t: t, issues: map[string]map[int]*fakeIssue{}, comments: map[string][]fakeIssueComment{}, status: map[string]int{}, bodies: map[string]map[string]any{}, tokens: map[string]int{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIssuesGitHub) add(repo string, issue *fakeIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issues[repo] == nil {
		f.issues[repo] = map[int]*fakeIssue{}
	}
	f.nextID++
	issue.id = f.nextID
	for _, event := range issue.timeline {
		f.nextID++
		event["id"] = f.nextID
	}
	f.issues[repo][issue.number] = issue
}

// event 在持有 mu 时追加唯一事件；保留秒粒度时间，避免测试依赖时间戳区分连续操作。
func (f *fakeIssuesGitHub) event(issue *fakeIssue, kind string) {
	f.nextID++
	issue.timeline = append(issue.timeline, map[string]any{"id": f.nextID, "event": kind, "created_at": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)})
}

func (f *fakeIssuesGitHub) get(repo string, n int) fakeIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	issue := *f.issues[repo][n]
	issue.labels = slices.Clone(issue.labels)
	issue.assignees = slices.Clone(issue.assignees)
	return issue
}

func (f *fakeIssuesGitHub) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakeIssuesGitHub) client() *githubapp.Client {
	return &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(f.t), APIURL: f.server.URL, HTTP: f.server.Client(), Cache: &githubapp.Cache{}}
}

func (i *fakeIssue) json(repo string) map[string]any {
	labels := []map[string]any{}
	for _, l := range i.labels {
		labels = append(labels, map[string]any{"name": l})
	}
	assignees := []map[string]any{}
	for _, a := range i.assignees {
		assignees = append(assignees, map[string]any{"login": a, "type": "User"})
	}
	v := map[string]any{
		"id":     i.id,
		"number": i.number, "title": i.title, "body": i.body, "state": i.state, "state_reason": i.stateReason,
		"html_url": "https://github.com/" + repo + "/issues/" + strconv.Itoa(i.number),
		"user":     map[string]any{"login": i.author, "type": "User"}, "labels": labels, "assignees": assignees, "comments": 0,
		"created_at": i.created.UTC().Format(time.RFC3339), "updated_at": i.created.UTC().Format(time.RFC3339), "closed_at": nil,
	}
	if i.pull {
		v["pull_request"] = map[string]any{"url": "https://api.github.com/repos/" + repo + "/pulls/" + strconv.Itoa(i.number)}
	}
	return v
}

func (f *fakeIssuesGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	before := f.before
	f.mu.Unlock()
	if before != nil {
		before(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: invalid JSON body", key)
		}
	}
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if key == "POST /app/installations/77/access_tokens" {
		repos, _ := body["repositories"].([]any)
		perms, _ := body["permissions"].(map[string]any)
		if len(repos) != 1 || perms["issues"] != "write" || perms["metadata"] != "read" || len(perms) != 2 {
			f.t.Errorf("token scope: %v", body)
		}
		f.tokens[repos[0].(string)]++
		reply(201, map[string]any{"token": "ghs_" + repos[0].(string), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		return
	}
	if r.Header.Get("X-GitHub-Api-Version") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_") {
		f.t.Errorf("%s: missing GitHub headers or installation token", key)
	}
	if r.Method != "GET" {
		f.writes = append(f.writes, key)
		f.bodies[key] = body
	}
	if status, ok := f.status[key]; ok {
		reply(status, map[string]string{"message": "override"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if len(parts) < 3 || parts[2] != "issues" {
		reply(404, map[string]string{"message": "Not Found"})
		return
	}
	repo := parts[0] + "/" + parts[1]
	issues, ok := f.issues[repo]
	if !ok {
		reply(404, map[string]string{"message": "Not Found"})
		return
	}
	if len(parts) == 3 {
		state := r.URL.Query().Get("state")
		var numbers []int
		for n, issue := range issues {
			if issue.state == state {
				numbers = append(numbers, n)
			}
		}
		slices.Sort(numbers)
		slices.Reverse(numbers)
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		start := min((page-1)*perPage, len(numbers))
		out := []map[string]any{}
		for _, n := range numbers[start:min(start+perPage, len(numbers))] {
			out = append(out, issues[n].json(repo))
		}
		reply(200, out)
		return
	}
	if parts[3] == "comments" {
		if q := r.URL.Query(); q.Get("direction") != "asc" || q.Get("sort") != "created" || q.Get("since") == "" {
			f.t.Errorf("comments read newest first or without a window: %s", r.URL.RawQuery)
		}
		out := []map[string]any{}
		for _, c := range f.comments[repo] {
			out = append(out, map[string]any{"issue_url": "https://api.github.com/repos/" + repo + "/issues/" + strconv.Itoa(c.number), "user": map[string]any{"login": c.login, "type": c.kind}, "author_association": c.association, "created_at": c.created.UTC().Format(time.RFC3339)})
		}
		reply(200, out)
		return
	}
	n, _ := strconv.Atoi(parts[3])
	issue, ok := issues[n]
	if !ok {
		reply(404, map[string]string{"message": "Not Found"})
		return
	}
	sub := strings.Join(parts[4:], "/")
	switch {
	case r.Method == "GET" && sub == "":
		reply(200, issue.json(repo))
	case r.Method == "GET" && sub == "timeline":
		// 与 GitHub 一样按 per_page 分页，多页时用 Link 指向最后一页。
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		events := issue.timeline
		if events == nil {
			events = []map[string]any{}
		}
		if last := (len(events) + perPage - 1) / perPage; last > 1 {
			w.Header().Set("Link", `<`+f.server.URL+r.URL.Path+`?per_page=`+strconv.Itoa(perPage)+`&page=2>; rel="next", <`+f.server.URL+r.URL.Path+`?per_page=`+strconv.Itoa(perPage)+`&page=`+strconv.Itoa(last)+`>; rel="last"`)
		}
		start := min((page-1)*perPage, len(events))
		reply(200, events[start:min(start+perPage, len(events))])
	case r.Method == "PATCH" && sub == "":
		oldState, oldReason := issue.state, issue.stateReason
		issue.state, _ = body["state"].(string)
		issue.stateReason, _ = body["state_reason"].(string)
		if issue.state != oldState || issue.stateReason != oldReason {
			kind := "closed"
			if issue.state == "open" {
				kind = "reopened"
			}
			f.event(issue, kind)
		}
		reply(200, issue.json(repo))
	case r.Method == "POST" && sub == "labels":
		for _, l := range body["labels"].([]any) {
			if !slices.Contains(issue.labels, l.(string)) {
				issue.labels = append(issue.labels, l.(string))
				f.event(issue, "labeled")
			}
		}
		reply(200, []any{})
	case r.Method == "DELETE" && strings.HasPrefix(sub, "labels/"):
		name := strings.TrimPrefix(sub, "labels/")
		if !slices.Contains(issue.labels, name) {
			reply(404, map[string]string{"message": "Label does not exist"})
			return
		}
		issue.labels = slices.DeleteFunc(issue.labels, func(l string) bool { return l == name })
		f.event(issue, "unlabeled")
		reply(200, []any{})
	case sub == "assignees" && (r.Method == "POST" || r.Method == "DELETE"):
		for _, a := range body["assignees"].([]any) {
			had := slices.Contains(issue.assignees, a.(string))
			issue.assignees = slices.DeleteFunc(issue.assignees, func(x string) bool { return x == a.(string) })
			if r.Method == "POST" {
				issue.assignees = append(issue.assignees, a.(string))
			}
			if had != (r.Method == "POST") {
				kind := "unassigned"
				if r.Method == "POST" {
					kind = "assigned"
				}
				f.event(issue, kind)
			}
		}
		reply(201, issue.json(repo))
	case r.Method == "POST" && sub == "comments":
		f.event(issue, "commented")
		reply(201, map[string]any{"id": 1, "body": body["body"]})
	default:
		reply(404, map[string]string{"message": "Not Found"})
	}
}

const (
	issuesTestAdminToken = "issues-admin-token-000000000000000000000000"
	issuesWinRepo        = "metasequoiaime/msime-windows"
	issuesCoreRepo       = "metasequoiaime/msime"
)

// issuesTestPlatforms maps Windows by repository and macOS and Linux by label.
var issuesTestPlatforms = []AdminPlatformConfig{
	{ID: "windows", Name: "Windows", Repo: issuesWinRepo, TagPrefix: "windows-v", Assignee: "houko", Label: "windows"},
	{ID: "macos", Name: "macOS", Repo: issuesCoreRepo, TagPrefix: "macos-v", Assignee: "fanlusky", Label: "macos"},
	{ID: "linux", Name: "Linux", Repo: issuesCoreRepo, TagPrefix: "linux-v", Label: "linux"},
}

// issuesFixture is a database-backed admin server whose GitHub is fake: msime-windows has issues 10 (new, bug), 11 (triaged) and pull request 12; msime has 20 (new, macOS idea), 21 (closed duplicate, Linux) and 22 (closed, no platform).
func issuesFixture(t *testing.T) (*Server, *fakeIssuesGitHub, *pgx.Conn, string) {
	t.Helper()
	conn, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_ADMIN_TOKEN", issuesTestAdminToken)
	s, err := New(Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin:   AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN", GitHub: AdminGitHubConfig{IssueRepos: []string{issuesWinRepo, issuesCoreRepo}, Platforms: issuesTestPlatforms}},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.CloseAccounts()
		s.Close()
	})
	f := newFakeIssuesGitHub(t)
	now := time.Now()
	f.add(issuesWinRepo, &fakeIssue{number: 10, title: "Win11 24H2 下 Word 中候选窗位置偏移", body: "候选窗出现在光标上方。", author: "alice", state: "open", labels: []string{"bug", "显示"}, created: now.Add(-3 * time.Hour),
		timeline: []map[string]any{
			{"event": "commented", "actor": map[string]any{"login": "houko"}, "body": "需要日志", "created_at": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
			{"event": "labeled", "actor": map[string]any{"login": "houko"}, "label": map[string]any{"name": "bug"}, "created_at": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
			{"event": "subscribed", "actor": map[string]any{"login": "houko"}, "created_at": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		}})
	f.add(issuesWinRepo, &fakeIssue{number: 11, title: "Win10 下 Word 候选窗位置偏移", author: "bob", state: "open", labels: []string{"bug", "triaged"}, assignees: []string{"houko"}, created: now.Add(-48 * time.Hour)})
	f.add(issuesWinRepo, &fakeIssue{number: 12, title: "fix: candidate window", author: "carol", state: "open", created: now.Add(-time.Hour), pull: true})
	f.add(issuesCoreRepo, &fakeIssue{number: 20, title: "希望支持按应用记住中英文状态", author: "dave", state: "open", labels: []string{"enhancement", "macos"}, created: now.Add(-10 * 24 * time.Hour)})
	f.add(issuesCoreRepo, &fakeIssue{number: 21, title: "Fcitx5 下 Shift 切换偶尔失效", author: "erin", state: "closed", stateReason: "not_planned", labels: []string{"bug", "linux", "duplicate"}, created: now.Add(-20 * 24 * time.Hour)})
	f.add(issuesCoreRepo, &fakeIssue{number: 22, title: "文档：Linux 安装步骤缺少依赖说明", author: "frank", state: "closed", stateReason: "completed", labels: []string{"documentation"}, created: now.Add(-40 * 24 * time.Hour)})
	// First responses: #10 after 1h by a maintainer (the author's own comment and a bot's do not count), #20 after 3h; #22 is older than the window.
	f.comments[issuesWinRepo] = []fakeIssueComment{
		{10, "alice", "User", "NONE", now.Add(-150 * time.Minute)},
		{10, "github-actions", "Bot", "NONE", now.Add(-170 * time.Minute)},
		{10, "houko", "User", "OWNER", now.Add(-2 * time.Hour)},
	}
	f.comments[issuesCoreRepo] = []fakeIssueComment{
		{20, "fanlusky", "User", "MEMBER", now.Add(-10*24*time.Hour + 3*time.Hour)},
		{20, "stranger", "User", "NONE", now.Add(-10*24*time.Hour + time.Hour)},
		{22, "fanlusky", "User", "MEMBER", now.Add(-39 * 24 * time.Hour)},
	}
	s.adminGitHub = f.client()
	return s, f, conn, schema
}

func issuesRequest(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "https://admin.example.com"+path, reader)
	r.Header.Set("Authorization", "Bearer "+issuesTestAdminToken)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s %s: invalid JSON %q", method, path, w.Body.String())
	}
	return w.Code, v
}

func issueErrorCode(v map[string]any) string {
	e, _ := v["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

type issueAuditRow struct {
	action, target, actor string
	detail                map[string]any
}

func issueAudits(t *testing.T, conn *pgx.Conn, schema string) []issueAuditRow {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT action,target,actor,detail FROM `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` WHERE action LIKE 'issue\_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []issueAuditRow
	for rows.Next() {
		var row issueAuditRow
		var raw []byte
		if err := rows.Scan(&row.action, &row.target, &row.actor, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &row.detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func issueNumbers(v map[string]any) []int {
	var out []int
	items, _ := v["items"].([]any)
	for _, item := range items {
		out = append(out, int(item.(map[string]any)["number"].(float64)))
	}
	return out
}

// Without admin.github every issue endpoint answers 404 github_disabled, and wrong methods are refused first.
func TestAdminIssuesGitHubDisabled(t *testing.T) {
	s, _ := adminRBACFixture(t)
	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/api/issues", 404, "github_disabled"},
		{"GET", "/api/issues/metasequoiaime/msime/1", 404, "github_disabled"},
		{"POST", "/api/issues/actions", 404, "github_disabled"},
		{"POST", "/api/issues", 405, "method_not_allowed"},
		{"GET", "/api/issues/actions", 405, "method_not_allowed"},
		{"DELETE", "/api/issues/metasequoiaime/msime/1", 405, "method_not_allowed"},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, tc.method, tc.path, "", strings.Repeat("a", 40)))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if s.searchIssues("x") != nil {
		t.Fatal("search without a listing returned hits")
	}
	if n, err := s.pendingIssues(context.Background()); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

// A role without triage_issues cannot change issues, and nothing reaches GitHub.
func TestAdminIssueActionsPermission(t *testing.T) {
	s, _ := adminRBACFixture(t)
	f := newFakeIssuesGitHub(t)
	s.adminGitHub = f.client()
	s.config.Admin.GitHub.IssueRepos = []string{issuesWinRepo}
	r := adminRequest(s, "POST", "/api/issues/actions", "", account.AdminTokenPrefix+"readonly")
	r.Body = io.NopCloser(strings.NewReader(`{"action":"close","items":[{"repo":"` + issuesWinRepo + `","n":1}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "permission_denied") || len(f.writeLog()) != 0 || len(f.tokens) != 0 {
		t.Fatal(w.Code, w.Body.String(), f.writeLog())
	}
}

func TestAdminIssuesList(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	status, v := issuesRequest(t, s, "GET", "/api/issues", "")
	if status != 200 {
		t.Fatal(status, v)
	}
	// The default filter is open issues, newest first, without the pull request.
	if got := issueNumbers(v); !slices.Equal(got, []int{10, 11, 20}) {
		t.Fatal("open issues", got)
	}
	items := v["items"].([]any)
	first := items[0].(map[string]any)
	if first["repo"] != issuesWinRepo || first["state"] != "new" || first["platform"] != "windows" || first["kind"] != "bug" || first["author"] != "alice" || first["url"] != "https://github.com/"+issuesWinRepo+"/issues/10" {
		t.Fatal("row", first)
	}
	if third := items[2].(map[string]any); third["platform"] != "macos" || third["kind"] != "idea" || items[1].(map[string]any)["state"] != "triaged" {
		t.Fatal("rows", items)
	}
	counts := v["platform_counts"].(map[string]any)
	if counts["all"] != 3.0 || counts["windows"] != 2.0 || counts["macos"] != 1.0 || counts["linux"] != 0.0 || counts["other"] != 0.0 {
		t.Fatal("platform counts", counts)
	}
	states := v["state_counts"].(map[string]any)
	if states["new"] != 2.0 || states["triaged"] != 1.0 || states["done"] != 1.0 || states["dup"] != 1.0 {
		t.Fatal("state counts", states)
	}
	stats := v["stats"].(map[string]any)
	// (1h + 3h) / 2 samples.
	if stats["pending"] != 2.0 || stats["triaged"] != 1.0 || stats["new_this_week"] != 2.0 || stats["first_response_samples"] != 2.0 || stats["first_response_hours"].(float64) < 1.99 || stats["first_response_hours"].(float64) > 2.01 {
		t.Fatal("stats", stats)
	}
	if v["total"] != 3.0 || v["has_more"] != false || len(v["platforms"].([]any)) != 3 || len(v["unavailable"].([]any)) != 0 {
		t.Fatal("page", v)
	}

	for _, tc := range []struct {
		query string
		want  []int
	}{
		{"?state=closed", []int{21, 22}}, {"?state=dup", []int{21}}, {"?state=done", []int{22}}, {"?state=all&platform=other", []int{22}},
		{"?state=new&platform=windows", []int{10}}, {"?state=all&platform=linux", []int{21}}, {"?state=open&page=2", nil},
	} {
		status, v := issuesRequest(t, s, "GET", "/api/issues"+tc.query, "")
		if status != 200 || !slices.Equal(issueNumbers(v), tc.want) {
			t.Error(tc.query, status, issueNumbers(v))
		}
	}
	for _, tc := range []struct{ query, code string }{
		{"?platform=windows-phone", "invalid_platform"}, {"?state=closedx", "invalid_state"}, {"?page=0", "invalid_page"}, {"?page=01", "invalid_page"}, {"?page=x", "invalid_page"},
	} {
		if status, v := issuesRequest(t, s, "GET", "/api/issues"+tc.query, ""); status != 400 || issueErrorCode(v) != tc.code {
			t.Error(tc.query, status, v)
		}
	}
	// Reads are cached: one token per repository for all of the requests above.
	if f.tokens[strings.Split(issuesWinRepo, "/")[1]] != 1 {
		t.Fatal("tokens", f.tokens)
	}

	// The shell badge and the global search use the same listing.
	if n, err := s.pendingIssues(context.Background()); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	hits := s.searchIssues("候选窗")
	if len(hits) != 2 || hits[0].ID != issuesWinRepo+"#10" || hits[0].Title != "#10 Win11 24H2 下 Word 中候选窗位置偏移" || hits[0].Target != "issues" || hits[0].Kind != "issue" {
		t.Fatal(hits)
	}
	if hits := s.searchIssues("#21"); len(hits) != 1 || hits[0].ID != issuesCoreRepo+"#21" {
		t.Fatal(hits)
	}
}

// One repository failing leaves the others listed and names it; all failing is an error.
func TestAdminIssuesListUnavailable(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	f.status["GET /repos/"+issuesCoreRepo+"/issues"] = 500
	status, v := issuesRequest(t, s, "GET", "/api/issues?state=all", "")
	if status != 200 || !slices.Equal(issueNumbers(v), []int{10, 11}) || len(v["unavailable"].([]any)) != 1 || v["unavailable"].([]any)[0] != issuesCoreRepo {
		t.Fatal(status, v)
	}
	f.status["GET /repos/"+issuesWinRepo+"/issues"] = 403
	s.adminGitHub = f.client()
	if status, v := issuesRequest(t, s, "GET", "/api/issues", ""); status != 502 || issueErrorCode(v) != "github_rejected" {
		t.Fatal(status, v)
	}
	if _, err := s.pendingIssues(context.Background()); err == nil {
		t.Fatal("pending count without any readable repository")
	}
}

// Issues created after the first complete listing are announced once; the backlog never is.
func TestAdminIssuesAnnounceNew(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	if _, err := s.pendingIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := s.issueMemory()
	if m.baseline.IsZero() || len(m.seen) != 0 {
		t.Fatal("first listing announced the backlog", m.seen)
	}
	f.add(issuesWinRepo, &fakeIssue{number: 13, title: "新问题", author: "gina", state: "open", created: time.Now().Add(time.Second)})
	s.adminGitHub = f.client()
	if n, err := s.pendingIssues(context.Background()); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if !m.seen[issuesWinRepo+"#13"] || len(m.seen) != 1 {
		t.Fatal(m.seen)
	}
	if got := notificationRows(t, conn, schema); !slices.Equal(got, []string{"issue issues " + issuesWinRepo + "#13"}) {
		t.Fatal("issue notifications", got)
	}
}

// 通知写入暂时失败后，同一问题应在后续轮询重试，成功后仍只保留一条通知。
func TestAdminIssuesAnnounceRetriesAfterDatabaseFailure(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	ctx := context.Background()
	if _, err := s.pendingIssues(ctx); err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "admin_notifications"}.Sanitize()
	if _, err := conn.Exec(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT reject_issue_notification CHECK (kind <> 'issue')"); err != nil {
		t.Fatal(err)
	}
	f.add(issuesWinRepo, &fakeIssue{number: 13, title: "新问题", author: "gina", state: "open", created: time.Now().Add(time.Second)})
	s.adminGitHub = f.client()
	if n, err := s.pendingIssues(ctx); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if got := notificationRows(t, conn, schema); len(got) != 0 {
		t.Fatal("失败的写入不应产生通知", got)
	}
	if _, err := conn.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT reject_issue_notification"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if n, err := s.pendingIssues(ctx); n != 3 || err != nil {
			t.Fatal(n, err)
		}
		if got := notificationRows(t, conn, schema); !slices.Equal(got, []string{"issue issues " + issuesWinRepo + "#13"}) {
			t.Fatalf("第 %d 次重试后通知错误：%v", i+1, got)
		}
	}
}

func TestAdminIssueDetail(t *testing.T) {
	s, _, _, _ := issuesFixture(t)
	status, v := issuesRequest(t, s, "GET", "/api/issues/"+issuesWinRepo+"/10", "")
	if status != 200 {
		t.Fatal(status, v)
	}
	issue := v["issue"].(map[string]any)
	if issue["number"] != 10.0 || issue["body"] != "候选窗出现在光标上方。" || issue["platform"] != "windows" || v["platform_assignee"] != "houko" {
		t.Fatal(v)
	}
	timeline := v["timeline"].([]any)
	if len(timeline) != 3 || timeline[0].(map[string]any)["kind"] != "created" || timeline[0].(map[string]any)["actor"] != "alice" ||
		timeline[1].(map[string]any)["kind"] != "commented" || timeline[1].(map[string]any)["text"] != "需要日志" || timeline[2].(map[string]any)["text"] != "bug" {
		t.Fatal("timeline", timeline)
	}
	similar := v["similar"].([]any)
	if len(similar) != 1 || similar[0].(map[string]any)["number"] != 11.0 || similar[0].(map[string]any)["state"] != "triaged" {
		t.Fatal("similar", similar)
	}
	// Repository names are matched case-insensitively against the configured list.
	if status, _ := issuesRequest(t, s, "GET", "/api/issues/MetasequoiaIME/msime/20", ""); status != 200 {
		t.Fatal(status)
	}
	for _, tc := range []struct {
		path   string
		status int
		code   string
	}{
		{"/api/issues/" + issuesWinRepo + "/12", 404, "not_found"},
		{"/api/issues/" + issuesWinRepo + "/99", 404, "not_found"},
		{"/api/issues/someone/else/1", 404, "not_found"},
		{"/api/issues/" + issuesWinRepo, 404, "not_found"},
		{"/api/issues/", 404, "not_found"},
		{"/api/issues/" + issuesWinRepo + "/0", 400, "invalid_id"},
		{"/api/issues/" + issuesWinRepo + "/010", 400, "invalid_id"},
		{"/api/issues/" + issuesWinRepo + "/10/extra", 404, "not_found"},
	} {
		if status, v := issuesRequest(t, s, "GET", tc.path, ""); status != tc.status || issueErrorCode(v) != tc.code {
			t.Error(tc.path, status, v)
		}
	}
}

func TestAdminIssueActions(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	item := func(repo string, n int) string { return `{"repo":"` + repo + `","n":` + strconv.Itoa(n) + `}` }
	act := func(action, items, body string) (int, map[string]any) {
		payload := `{"action":"` + action + `","items":[` + items + `]`
		if body != "" {
			payload += `,"body":` + strconv.Quote(body)
		}
		return issuesRequest(t, s, "POST", "/api/issues/actions", payload+"}")
	}

	// List first so the cache is warm; a write must invalidate it.
	issuesRequest(t, s, "GET", "/api/issues", "")
	status, v := act("triage", item(issuesWinRepo, 10)+","+item("METASEQUOIAIME/msime", 20), "")
	if status != 200 || v["affected"] != 2.0 || len(v["failed"].([]any)) != 0 {
		t.Fatal(status, v)
	}
	if got := f.get(issuesWinRepo, 10); !slices.Contains(got.labels, "triaged") || !slices.Equal(got.assignees, []string{"houko"}) {
		t.Fatal("triage", got)
	}
	if got := f.get(issuesCoreRepo, 20); !slices.Contains(got.labels, "triaged") || !slices.Equal(got.assignees, []string{"fanlusky"}) {
		t.Fatal("triage macOS", got)
	}
	if _, v := issuesRequest(t, s, "GET", "/api/issues?state=new", ""); len(issueNumbers(v)) != 0 {
		t.Fatal("listing still cached after triage", issueNumbers(v))
	}
	audits := issueAudits(t, conn, schema)
	if len(audits) != 2 || audits[0].action != "issue_triage" || audits[0].target != issuesWinRepo+"#10" || audits[0].actor != "legacy-token" ||
		audits[0].detail["from"] != "new" || audits[0].detail["to"] != "triaged" || audits[0].detail["assignee"] != "houko" || audits[0].detail["platform"] != "windows" || audits[1].target != issuesCoreRepo+"#20" {
		t.Fatal("audit", audits)
	}

	// untriage is triage's undo: the label and the platform assignee go.
	if status, v := act("untriage", item(issuesWinRepo, 10), ""); status != 200 || v["affected"] != 1.0 {
		t.Fatal(status, v)
	}
	if got := f.get(issuesWinRepo, 10); slices.Contains(got.labels, "triaged") || len(got.assignees) != 0 {
		t.Fatal("untriage", got)
	}

	// mark_dup closes with the duplicate label; reopen undoes it and keeps triaged.
	if status, _ := act("mark_dup", item(issuesWinRepo, 11), ""); status != 200 {
		t.Fatal(status)
	}
	if got := f.get(issuesWinRepo, 11); got.state != "closed" || got.stateReason != "not_planned" || !slices.Contains(got.labels, "duplicate") {
		t.Fatal("mark_dup", got)
	}
	if status, _ := act("reopen", item(issuesWinRepo, 11), ""); status != 200 {
		t.Fatal(status)
	}
	if got := f.get(issuesWinRepo, 11); got.state != "open" || got.stateReason != "reopened" || slices.Contains(got.labels, "duplicate") || !slices.Contains(got.labels, "triaged") {
		t.Fatal("reopen", got)
	}
	if status, _ := act("close", item(issuesWinRepo, 10), ""); status != 200 {
		t.Fatal(status)
	}
	if got := f.get(issuesWinRepo, 10); got.state != "closed" || got.stateReason != "completed" {
		t.Fatal("close", got)
	}
	if status, _ := act("comment", item(issuesWinRepo, 10), "感谢反馈！"); status != 200 || f.bodies["POST /repos/"+issuesWinRepo+"/issues/10/comments"]["body"] != "感谢反馈！" {
		t.Fatal(status, f.bodies)
	}
	audits = issueAudits(t, conn, schema)
	var actions []string
	for _, a := range audits {
		actions = append(actions, a.action)
	}
	if !slices.Equal(actions, []string{"issue_triage", "issue_triage", "issue_untriage", "issue_mark_dup", "issue_reopen", "issue_close", "issue_comment"}) ||
		audits[4].detail["from"] != "dup" || audits[4].detail["to"] != "triaged" || audits[6].detail["length"] != 5.0 {
		t.Fatal("audit", audits)
	}

	// A partly failing batch reports the failures; a wholly failing one is an error without audit rows.
	status, v = act("close", item(issuesWinRepo, 11)+","+item(issuesWinRepo, 99)+","+item(issuesWinRepo, 12), "")
	failed, _ := v["failed"].([]any)
	if status != 200 || v["affected"] != 1.0 || len(failed) != 2 || failed[0].(map[string]any)["code"] != "not_found" || failed[1].(map[string]any)["n"] != 12.0 {
		t.Fatal(status, v)
	}
	before := len(issueAudits(t, conn, schema))
	if status, v := act("reopen", item(issuesWinRepo, 99), ""); status != 404 || issueErrorCode(v) != "not_found" {
		t.Fatal(status, v)
	}
	f.status["PATCH /repos/"+issuesWinRepo+"/issues/11"] = 422
	if status, v := act("reopen", item(issuesWinRepo, 11), ""); status != 502 || issueErrorCode(v) != "github_rejected" {
		t.Fatal(status, v)
	}
	f.status["POST /repos/"+issuesWinRepo+"/issues/11/comments"] = 403
	if status, v := act("comment", item(issuesWinRepo, 11), "hi"); status != 502 || issueErrorCode(v) != "github_rejected" {
		t.Fatal(status, v)
	}
	if after := len(issueAudits(t, conn, schema)); after != before {
		t.Fatal("failed actions were audited", before, after)
	}

	writes := len(f.writeLog())
	for _, tc := range []struct{ body, code string }{
		{`{"action":"delete","items":[` + item(issuesWinRepo, 10) + `]}`, "invalid_action"},
		{`{"action":"close","items":[]}`, "invalid_items"},
		{`{"action":"close"}`, "invalid_items"},
		{`{"action":"close","items":[` + item("someone/else", 1) + `]}`, "invalid_items"},
		{`{"action":"close","items":[` + item(issuesWinRepo, 0) + `]}`, "invalid_items"},
		{`{"action":"close","items":[` + item(issuesWinRepo, 10) + `,` + item("MetasequoiaIME/msime-windows", 10) + `]}`, "invalid_items"},
		{`{"action":"close","items":[` + item(issuesWinRepo, 10) + `],"body":"x"}`, "invalid_body"},
		{`{"action":"comment","items":[` + item(issuesWinRepo, 10) + `],"body":"  "}`, "invalid_body"},
		{`{"action":"comment","items":[` + item(issuesWinRepo, 10) + `]}`, "invalid_body"},
		{`{"action":"close","items":[` + item(issuesWinRepo, 10) + `],"extra":1}`, "invalid_json"},
	} {
		if status, v := issuesRequest(t, s, "POST", "/api/issues/actions", tc.body); status != 400 || issueErrorCode(v) != tc.code {
			t.Error(tc.body, status, v)
		}
	}
	many := make([]string, issueMaxItems+1)
	for i := range many {
		many[i] = item(issuesWinRepo, i+1)
	}
	if status, v := act("close", strings.Join(many, ","), ""); status != 400 || issueErrorCode(v) != "invalid_items" {
		t.Fatal(status, v)
	}
	// A comment longer than GitHub's 65536 characters is invalid_body, and a long CJK comment within that limit is accepted although it is larger than 64 KiB.
	if status, v := act("comment", item(issuesWinRepo, 10), strings.Repeat("x", issueCommentMax+1)); status != 400 || issueErrorCode(v) != "invalid_body" {
		t.Fatal(status, v)
	}
	if status, v := act("comment", item(issuesWinRepo, 10), strings.Repeat("长", issueCommentMax+1)); status != 400 || issueErrorCode(v) != "invalid_body" {
		t.Fatal(status, v)
	}
	if len(f.writeLog()) != writes {
		t.Fatal("an invalid request reached GitHub", f.writeLog()[writes:])
	}
	long := strings.Repeat("长", 30000)
	if status, v := act("comment", item(issuesWinRepo, 10), long); status != 200 || f.bodies["POST /repos/"+issuesWinRepo+"/issues/10/comments"]["body"] != long {
		t.Fatal("a 90 KB comment within GitHub's character limit", status, v)
	}
}

func TestIssueHelpers(t *testing.T) {
	for _, tc := range []struct {
		state, reason string
		labels        []string
		want          string
	}{
		{"open", "", nil, "new"}, {"open", "", []string{"Triaged"}, "triaged"}, {"closed", "completed", []string{"triaged"}, "done"},
		{"closed", "duplicate", nil, "dup"}, {"closed", "not_planned", []string{"duplicate"}, "dup"},
	} {
		if got := issueState(tc.state, tc.reason, tc.labels); got != tc.want {
			t.Error(tc, got)
		}
	}
	if issueKind([]string{"Bug"}) != "bug" || issueKind([]string{"enhancement"}) != "idea" || issueKind([]string{"documentation"}) != "docs" || issueKind([]string{"显示"}) != "" {
		t.Fatal("kind")
	}
	if a, b := titleBigrams("Win11 下 Word 候选窗偏移"), titleBigrams("Win10 下 Word 候选窗偏移"); bigramSimilarity(a, b) < 0.7 {
		t.Fatal(bigramSimilarity(a, b))
	}
	if bigramSimilarity(titleBigrams("九宫格长按退格"), titleBigrams("Shift 切换失效")) != 0 || bigramSimilarity(titleBigrams(""), titleBigrams("x")) != 0 {
		t.Fatal("unrelated titles are similar")
	}
	s := &Server{config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{Platforms: issuesTestPlatforms}}}}
	if p := s.issuePlatform(issuesWinRepo, nil); p == nil || p.ID != "windows" {
		t.Fatal("repository fallback", p)
	}
	if p := s.issuePlatform(issuesCoreRepo, nil); p != nil {
		t.Fatal("a repository shared by platforms picked one", p)
	}
	if p := s.issuePlatform(issuesCoreRepo, []string{"Linux"}); p == nil || p.ID != "linux" {
		t.Fatal("label", p)
	}
}

// 后续写入明确被拒绝时回滚前面的步骤且不记审计；回滚也失败时，记录尚未撤销的部分变更。
func TestAdminIssueActionRollback(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	act := func(action string, n int) (int, map[string]any) {
		return issuesRequest(t, s, "POST", "/api/issues/actions", `{"action":"`+action+`","items":[{"repo":"`+issuesWinRepo+`","n":`+strconv.Itoa(n)+`}]}`)
	}
	f.status["POST /repos/"+issuesWinRepo+"/issues/10/assignees"] = 422
	if status, v := act("triage", 10); status != 502 || issueErrorCode(v) != "github_rejected" {
		t.Fatal(status, v)
	}
	if got := f.get(issuesWinRepo, 10); slices.Contains(got.labels, "triaged") || len(got.assignees) != 0 {
		t.Fatal("triage was not rolled back", got)
	}
	if audits := issueAudits(t, conn, schema); len(audits) != 0 {
		t.Fatal("a rolled back action was audited", audits)
	}

	// The duplicate label is removed first and put back when the reopen itself fails.
	if status, _ := act("mark_dup", 11); status != 200 {
		t.Fatal(status)
	}
	f.status["PATCH /repos/"+issuesWinRepo+"/issues/11"] = 422
	if status, _ := act("reopen", 11); status != 502 {
		t.Fatal(status)
	}
	if got := f.get(issuesWinRepo, 11); got.state != "closed" || !slices.Contains(got.labels, "duplicate") {
		t.Fatal("reopen was not rolled back", got)
	}
	delete(f.status, "PATCH /repos/"+issuesWinRepo+"/issues/11")

	// The rollback of the triaged label fails too: the label stays, so the request fails but the change is audited as partial.
	f.status["DELETE /repos/"+issuesWinRepo+"/issues/10/labels/triaged"] = 500
	if status, v := act("triage", 10); status != 502 || issueErrorCode(v) != "github_uncertain" {
		t.Fatal(status, v)
	}
	audits := issueAudits(t, conn, schema)
	last := audits[len(audits)-1]
	if len(audits) != 2 || last.action != "issue_triage" || last.target != issuesWinRepo+"#10" || last.detail["partial"] != true || last.detail["uncertain"] != true {
		t.Fatal("partial change not audited", audits)
	}
}

// Undoing the triage of an issue whose platform assignee was already assigned keeps that assignment; keep_assignee belongs to untriage only.
func TestAdminIssueUntriageKeepAssignee(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	body := func(action string, keep bool) string {
		return `{"action":"` + action + `","items":[{"repo":"` + issuesWinRepo + `","n":11,"keep_assignee":` + strconv.FormatBool(keep) + `}]}`
	}
	if status, v := issuesRequest(t, s, "POST", "/api/issues/actions", body("untriage", true)); status != 200 || v["affected"] != 1.0 {
		t.Fatal(status, v)
	}
	if got := f.get(issuesWinRepo, 11); slices.Contains(got.labels, "triaged") || !slices.Equal(got.assignees, []string{"houko"}) {
		t.Fatal("keep_assignee dropped the assignee", got)
	}
	writes := len(f.writeLog())
	for _, action := range []string{"triage", "close", "reopen", "mark_dup"} {
		if status, v := issuesRequest(t, s, "POST", "/api/issues/actions", body(action, true)); status != 400 || issueErrorCode(v) != "invalid_items" {
			t.Error(action, status, v)
		}
	}
	if len(f.writeLog()) != writes {
		t.Fatal("an invalid request reached GitHub")
	}
	if status, _ := issuesRequest(t, s, "POST", "/api/issues/actions", body("untriage", false)); status != 200 {
		t.Fatal(status)
	}
}

// A timeline longer than one page shows its newest page, so the latest replies are always there.
func TestAdminIssueDetailNewestTimelinePage(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	start := time.Now().Add(-200 * time.Hour)
	var events []map[string]any
	for i := range 150 {
		events = append(events, map[string]any{"event": "commented", "actor": map[string]any{"login": "houko"}, "body": "reply " + strconv.Itoa(i), "created_at": start.Add(time.Duration(i) * time.Hour).UTC().Format(time.RFC3339)})
	}
	f.add(issuesWinRepo, &fakeIssue{number: 30, title: "很长的讨论", author: "alice", state: "open", created: start, timeline: events})
	status, v := issuesRequest(t, s, "GET", "/api/issues/"+issuesWinRepo+"/30", "")
	if status != 200 || v["timeline_truncated"] != true {
		t.Fatal(status, v["timeline_truncated"])
	}
	timeline := v["timeline"].([]any)
	if len(timeline) != 51 || timeline[len(timeline)-1].(map[string]any)["text"] != "reply 149" || timeline[1].(map[string]any)["text"] != "reply 100" {
		t.Fatal(len(timeline), timeline[len(timeline)-1])
	}
	if status, v := issuesRequest(t, s, "GET", "/api/issues/"+issuesWinRepo+"/10", ""); status != 200 || v["timeline_truncated"] != false {
		t.Fatal(status, v["timeline_truncated"])
	}
	h := http.Header{}
	if issueLastPage(h) != 0 {
		t.Fatal("no Link header")
	}
	h.Set("Link", `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?per_page=100&page=7>; rel="last"`)
	if issueLastPage(h) != 7 {
		t.Fatal(issueLastPage(h))
	}
}

// setBefore 设置 fakeIssuesGitHub 处理请求前的钩子。
func (f *fakeIssuesGitHub) setBefore(hook func(r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.before = hook
}

// issueReadKind 把一个列表读取归为 open、closed 或 comments，其他请求为空串。
func issueReadKind(r *http.Request) string {
	switch {
	case r.Method != "GET":
		return ""
	case strings.HasSuffix(r.URL.Path, "/issues/comments"):
		return "comments"
	case strings.HasSuffix(r.URL.Path, "/issues"):
		return r.URL.Query().Get("state")
	}
	return ""
}

// 外壳角标只读未关闭 issue：评论读取一直阻塞（远超 5 秒预算）时，角标照样在预算内给出待分诊数，也不去读已关闭 issue；评论放行后问题列表仍有响应时间，并复用角标已缓存的未关闭 issue 分页。
func TestAdminIssuesBadgeSkipsComments(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseComments := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseComments)
	var mu sync.Mutex
	reads := map[string]int{}
	f.setBefore(func(r *http.Request) {
		kind := issueReadKind(r)
		mu.Lock()
		reads[kind]++
		mu.Unlock()
		if kind == "comments" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	})
	readCount := func(kind string) int {
		mu.Lock()
		defer mu.Unlock()
		return reads[kind]
	}
	// 与 adminShell 相同的预算。
	bounded, cancel := context.WithTimeout(context.Background(), adminShellGitHubBudget)
	defer cancel()
	if n, err := s.pendingIssues(bounded); n != 2 || err != nil {
		t.Fatal("badge", n, err)
	}
	if readCount("comments") != 0 || readCount("closed") != 0 || readCount("open") != 2 {
		t.Fatal("badge reads", reads)
	}
	// 第一次快照设定基线，之后新建的 issue 才会通知。
	if s.issueMemory().baseline.IsZero() {
		t.Fatal("the badge did not set the announcement baseline")
	}

	releaseComments()
	status, v := issuesRequest(t, s, "GET", "/api/issues", "")
	if stats, _ := v["stats"].(map[string]any); status != 200 || stats["first_response_samples"] != 2.0 || stats["pending"] != 2.0 {
		t.Fatal("issues page", status, v)
	}
	if readCount("open") != 2 || readCount("closed") != 2 || readCount("comments") != 2 {
		t.Fatal("the issues page did not reuse the badge's cached open pages", reads)
	}

	// 角标只读未关闭 issue，但不会把之前列出的已关闭 issue 从全局搜索中挤掉。
	s.adminGitHub = f.client()
	if n, err := s.pendingIssues(context.Background()); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	if hits := s.searchIssues("#21"); len(hits) != 1 || hits[0].ID != issuesCoreRepo+"#21" {
		t.Fatal("closed issue dropped from search", hits)
	}
	if hits := s.searchIssues("候选窗"); len(hits) != 2 {
		t.Fatal("open issues in search", hits)
	}
	// 重新打开的 issue 以角标这次读到的状态为准，不会以旧的已关闭状态重复出现。
	f.mu.Lock()
	f.issues[issuesCoreRepo][22].state = "open"
	f.mu.Unlock()
	s.adminGitHub = f.client()
	if n, err := s.pendingIssues(context.Background()); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	m := s.issueMemory()
	m.mu.Lock()
	var states []string
	for _, row := range m.items {
		if row.Number == 22 {
			states = append(states, row.State)
		}
	}
	m.mu.Unlock()
	if !slices.Equal(states, []string{"new"}) {
		t.Fatal("reopened issue in search", states)
	}
}

// 同一仓库的未关闭 issue、已关闭 issue 和评论三组读取并发进行：每组的第一个请求都要等另外两组到齐才返回，顺序读取会在这里卡住。
func TestAdminIssuesReadsRunConcurrently(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	var mu sync.Mutex
	arrived := map[string]bool{}
	together := make(chan struct{})
	f.setBefore(func(r *http.Request) {
		kind := issueReadKind(r)
		if kind == "" || !strings.HasPrefix(r.URL.Path, "/repos/"+issuesWinRepo+"/") {
			return
		}
		mu.Lock()
		first := !arrived[kind]
		arrived[kind] = true
		if first && len(arrived) == 3 {
			close(together)
		}
		mu.Unlock()
		if !first {
			return
		}
		select {
		case <-together:
		case <-time.After(10 * time.Second):
			t.Errorf("%s read of %s waited for the other reads: they run one after another", kind, issuesWinRepo)
		}
	})
	status, v := issuesRequest(t, s, "GET", "/api/issues?state=all", "")
	if stats, _ := v["stats"].(map[string]any); status != 200 || stats["first_response_samples"] != 2.0 || !slices.Equal(issueNumbers(v), []int{10, 11, 20, 21, 22}) {
		t.Fatal(status, v)
	}
}

// issueCaptureLogs 把默认 logger 换成写入缓冲区的 JSON logger，测试结束时还原。
func issueCaptureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var record map[string]any
			if line != "" && json.Unmarshal([]byte(line), &record) == nil {
				out = append(out, record)
			}
		}
		buf.Reset()
		return out
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// 只因 ctx 到期或取消而中断的 GitHub 调用记 WARN，GitHub 的错误状态和无法解码的正文记 ERROR；日志都带仓库名和调用名。
func TestIssueGitHubFailureLogLevel(t *testing.T) {
	logs := issueCaptureLogs(t)
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	live := context.Background()
	decodeErr := json.Unmarshal([]byte("{"), &struct{}{})
	for _, tc := range []struct {
		name  string
		err   func() error
		level string
		msg   string
	}{
		{"deadline", func() error {
			return issueResponseError(expired, issuesWinRepo, "list comments", githubapp.Response{Status: 200}, context.DeadlineExceeded)
		}, "WARN", "issues: GitHub unavailable"},
		{"canceled", func() error {
			return issueResponseError(live, issuesWinRepo, "list comments", githubapp.Response{}, context.Canceled)
		}, "WARN", "issues: GitHub unavailable"},
		{"server error", func() error {
			return issueResponseError(live, issuesWinRepo, "list comments", githubapp.Response{Status: 502}, nil)
		}, "ERROR", "issues: GitHub unavailable"},
		{"decode", func() error {
			return issueResponseError(live, issuesWinRepo, "list comments", githubapp.Response{Status: 200}, decodeErr)
		}, "ERROR", "issues: GitHub unavailable"},
		{"refused", func() error {
			return issueResponseError(live, issuesWinRepo, "list comments", githubapp.Response{Status: 403}, nil)
		}, "ERROR", "issues: GitHub refused"},
		// githubapp 铸造令牌时把原因转成了字符串，只能从 ctx 看出是超时。
		{"token deadline", func() error {
			return issueTokenError(expired, issuesWinRepo, errors.New("github unavailable: mint token: context deadline exceeded"))
		}, "WARN", "issues: GitHub installation token"},
		{"token rejected", func() error { return issueTokenError(live, issuesWinRepo, githubapp.ErrRejected) }, "ERROR", "issues: GitHub installation token"},
	} {
		if tc.err() == nil {
			t.Fatal(tc.name, "no error")
		}
		records := logs()
		if len(records) != 1 || records[0]["level"] != tc.level || records[0]["msg"] != tc.msg || records[0]["repo"] != issuesWinRepo {
			t.Errorf("%s: %v", tc.name, records)
		}
		if tc.msg != "issues: GitHub installation token" && records[0]["call"] != "list comments" {
			t.Errorf("%s: call missing: %v", tc.name, records)
		}
	}
}

// 端到端：评论分页读到超时时，日志是带仓库名和调用名的 WARN，不是 ERROR。
func TestAdminIssuesCommentDeadlineLogsWarn(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	// 先读一次列表，让令牌和 issue 分页进入缓存，之后只有评论需要访问 GitHub。
	if _, err := s.issueSnapshot(context.Background(), s.adminGitHub, issueScopeListing); err != nil {
		t.Fatal(err)
	}
	f.setBefore(func(r *http.Request) {
		if issueReadKind(r) == "comments" {
			<-r.Context().Done()
		}
	})
	logs := issueCaptureLogs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := s.issueSnapshot(ctx, s.adminGitHub, issueScopeFull); err == nil {
		t.Fatal("snapshot without any readable comments")
	}
	repos := map[string]bool{}
	for _, record := range logs() {
		if record["msg"] != "issues: GitHub unavailable" {
			continue
		}
		if record["level"] != "WARN" || record["call"] != "list comments" {
			t.Errorf("deadline logged as %v", record)
		}
		repo, _ := record["repo"].(string)
		repos[repo] = true
	}
	if !repos[issuesWinRepo] || !repos[issuesCoreRepo] {
		t.Fatal("deadline logs do not name every repository", repos)
	}
}
