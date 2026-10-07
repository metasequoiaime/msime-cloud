package server

import (
	"context"
	"encoding/json"
	"io"
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

// fakeReleaseGitHub serves the GitHub REST calls the release page makes. Releases are held per repository and PATCH applies to them, so a read after a write sees it.
type fakeReleaseGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	server   *httptest.Server
	releases map[string][]map[string]any
	runs     map[string][]map[string]any
	latest   map[string]int64
	status   map[string]int
	calls    map[string]int
	bodies   map[string][]map[string]any
}

func newFakeReleaseGitHub(t *testing.T) *fakeReleaseGitHub {
	t.Helper()
	f := &fakeReleaseGitHub{t: t, releases: map[string][]map[string]any{}, runs: map[string][]map[string]any{}, latest: map[string]int64{}, status: map[string]int{}, calls: map[string]int{}, bodies: map[string][]map[string]any{}}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	published := func(day string) string { return day + "T08:00:00Z" }
	f.releases["metasequoiaime/msime-windows"] = []map[string]any{
		{"id": 9, "tag_name": "macos-v1.0.0", "draft": false, "prerelease": false, "body": "", "created_at": published("2026-09-29"), "published_at": published("2026-09-29"), "author": map[string]any{"login": "fanlusky"}, "assets": []any{}},
		{"id": 4, "tag_name": "windows-v0.5.5", "draft": true, "prerelease": false, "body": "### 待办\n- 签名证书", "target_commitish": "main", "created_at": published("2026-09-30"), "published_at": nil, "author": map[string]any{"login": "houko"}, "assets": []any{}},
		{"id": 3, "tag_name": "windows-v0.5.4", "draft": false, "prerelease": false, "body": "### 修复\r\n- Win11 24H2 候选窗偏移\r\n\r\n### 新增\r\n* 剪贴板历史支持固定条目\r\n", "created_at": published("2026-09-25"), "published_at": published("2026-09-26"), "html_url": "https://github.com/metasequoiaime/msime-windows/releases/tag/windows-v0.5.4", "author": map[string]any{"login": "houko"},
			"assets": []any{map[string]any{"name": "msime-windows-x64-setup.exe", "size": 19084083, "download_count": 100, "browser_download_url": "https://example.test/x64"}, map[string]any{"name": "msime-windows-arm64-setup.exe", "size": 18454937, "download_count": 20}}},
		{"id": 2, "tag_name": "windows-v0.5.3", "draft": false, "prerelease": false, "body": "### 修复\n- Word 中 Shift 切换偶尔失效", "created_at": published("2026-09-12"), "published_at": published("2026-09-12"), "author": map[string]any{"login": "houko"},
			"assets": []any{map[string]any{"name": "msime-windows-x64-setup.exe", "size": 18874368, "download_count": 50}}},
		{"id": 1, "tag_name": "windows-v0.5.2", "draft": false, "prerelease": true, "body": releaseWithdrawnMarker + "\n" + releaseWithdrawnNotice + "\n\n### 说明\n- 缺少 vc_redist", "created_at": published("2026-08-30"), "published_at": published("2026-08-30"), "author": map[string]any{"login": "fanlusky"},
			"assets": []any{map[string]any{"name": "msime-windows-x64-setup.exe", "size": 18769510, "download_count": 7}}},
	}
	f.latest["metasequoiaime/msime-windows"] = 3
	f.runs["metasequoiaime/msime-windows@main"] = []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}}
	f.runs["metasequoiaime/msime-windows@windows-v0.5.4"] = []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}, {"name": "sign", "status": "in_progress", "conclusion": nil}}
	f.releases["metasequoiaime/msime"] = []map[string]any{}
	return f
}

func (f *fakeReleaseGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls[key]++
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: invalid JSON body %q", key, raw)
		}
		f.bodies[key] = append(f.bodies[key], body)
	}
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if status, ok := f.status[key]; ok {
		write(status, map[string]string{"message": "forced"})
		return
	}
	if r.Method == "POST" && r.URL.Path == "/app/installations/77/access_tokens" {
		write(201, map[string]any{"token": "release-token", "expires_at": time.Now().Add(time.Hour)})
		return
	}
	if r.Header.Get("Authorization") != "Bearer release-token" {
		write(401, nil)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if len(parts) < 2 {
		write(404, nil)
		return
	}
	repo := parts[0] + "/" + parts[1]
	releases, known := f.releases[repo]
	if !known {
		write(404, map[string]string{"message": "Not Found"})
		return
	}
	rest := strings.Join(parts[2:], "/")
	switch {
	case r.Method == "GET" && rest == "":
		write(200, map[string]any{"default_branch": "main"})
	case r.Method == "GET" && rest == "releases":
		if r.URL.Query().Get("per_page") != "100" {
			f.t.Errorf("releases listed with %q", r.URL.RawQuery)
		}
		write(200, releases)
	case r.Method == "GET" && rest == "releases/latest":
		if f.latest[repo] == 0 {
			write(404, nil)
			return
		}
		write(200, map[string]any{"id": f.latest[repo]})
	case r.Method == "GET" && strings.HasPrefix(rest, "commits/") && strings.HasSuffix(rest, "/check-runs"):
		ref := strings.TrimSuffix(strings.TrimPrefix(rest, "commits/"), "/check-runs")
		runs := f.runs[repo+"@"+ref]
		write(200, map[string]any{"total_count": len(runs), "check_runs": runs})
	case r.Method == "POST" && strings.HasPrefix(rest, "actions/workflows/") && strings.HasSuffix(rest, "/dispatches"):
		w.WriteHeader(204)
	case r.Method == "PATCH" && strings.HasPrefix(rest, "releases/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(rest, "releases/"), 10, 64)
		body := f.bodies[key][len(f.bodies[key])-1]
		for _, release := range releases {
			if int64(release["id"].(int)) != id {
				continue
			}
			for _, field := range []string{"body", "prerelease"} {
				if v, ok := body[field]; ok {
					release[field] = v
				}
			}
			if body["make_latest"] == "true" {
				f.latest[repo] = id
			}
			write(200, release)
			return
		}
		write(404, nil)
	default:
		write(404, nil)
	}
}

func (f *fakeReleaseGitHub) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeReleaseGitHub) lastBody(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies[key]) == 0 {
		return nil
	}
	return f.bodies[key][len(f.bodies[key])-1]
}

func (f *fakeReleaseGitHub) client(t *testing.T) *githubapp.Client {
	return &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: f.server.URL, HTTP: f.server.Client(), Cache: &githubapp.Cache{}}
}

var releaseTestPlatforms = []AdminPlatformConfig{
	{ID: "windows", Name: "Windows", Repo: "metasequoiaime/msime-windows", TagPrefix: "windows-v", ReleaseWorkflow: "release.yml"},
	{ID: "linux", Name: "Linux", Repo: "metasequoiaime/msime", TagPrefix: "linux-v"},
	{ID: "ghost", Name: "Ghost", Repo: "metasequoiaime/missing", TagPrefix: "ghost-v"},
}

type releaseListBody struct {
	Platforms []struct {
		ID        string         `json:"id"`
		Workflow  string         `json:"workflow"`
		Latest    *releaseView   `json:"latest"`
		Checklist []releaseCheck `json:"checklist"`
		Error     string         `json:"error"`
	} `json:"platforms"`
}

func serveAdminJSON(t *testing.T, s *Server, r *http.Request, want int, out any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", r.Method, r.URL.Path, w.Code, want, w.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

func errorCode(t *testing.T, s *Server, r *http.Request, want int) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	serveAdminJSON(t, s, r, want, &body)
	return body.Error.Code
}

// Every role reads the release list and history; each platform reports its newest release that is not withdrawn, the checklist of that release, or its own error without hiding the other platforms.
func TestAdminReleasesRead(t *testing.T) {
	s, _ := adminRBACFixture(t)
	s.config.Admin.GitHub.Platforms = releaseTestPlatforms
	readonly := account.AdminTokenPrefix + "readonly"
	if code := errorCode(t, s, adminRequest(s, "GET", "/api/releases", "", readonly), 404); code != "github_disabled" {
		t.Fatal(code)
	}
	f := newFakeReleaseGitHub(t)
	s.adminGitHub = f.client(t)

	var list releaseListBody
	serveAdminJSON(t, s, adminRequest(s, "GET", "/api/releases", "", readonly), 200, &list)
	if len(list.Platforms) != 3 {
		t.Fatalf("%+v", list)
	}
	windows, linux, ghost := list.Platforms[0], list.Platforms[1], list.Platforms[2]
	if windows.ID != "windows" || windows.Workflow != "release.yml" || windows.Latest == nil || windows.Latest.Tag != "windows-v0.5.5" || windows.Latest.Status != "draft" || windows.Latest.Version != "v0.5.5" || windows.Latest.PublishedAt != nil {
		t.Fatalf("windows: %+v", windows)
	}
	// The draft's checks ran on its target branch; no sign run there, so no 签名与公证 row; the store step needs a person until the release is public.
	want := []releaseCheck{{Key: "ci", Label: "CI 全部通过", State: "passed"}, {Key: "notes", Label: "更新日志已填写", State: "passed"}, {Key: "store", Label: "商店 / 分发渠道", State: "manual", Note: "需手动"}}
	if !jsonEqual(windows.Checklist, want) {
		t.Fatalf("checklist %+v", windows.Checklist)
	}
	if linux.Latest != nil || len(linux.Checklist) != 0 || linux.Error != "" {
		t.Fatalf("linux: %+v", linux)
	}
	if ghost.Error != "github_repo_not_found" || ghost.Latest != nil {
		t.Fatalf("ghost: %+v", ghost)
	}

	var history struct {
		Platform releasePlatformView `json:"platform"`
		Releases []releaseView       `json:"releases"`
	}
	serveAdminJSON(t, s, adminRequest(s, "GET", "/api/releases/windows", "", readonly), 200, &history)
	var tags, statuses []string
	for _, r := range history.Releases {
		tags = append(tags, r.Tag)
		statuses = append(statuses, r.Status)
	}
	if strings.Join(tags, ",") != "windows-v0.5.5,windows-v0.5.4,windows-v0.5.3,windows-v0.5.2" || strings.Join(statuses, ",") != "draft,released,released,withdrawn" || history.Platform.TagPrefix != "windows-v" {
		t.Fatal(tags, statuses, history.Platform)
	}
	current := history.Releases[1]
	if current.Downloads != 120 || len(current.Assets) != 2 || current.Assets[0].URL != "https://example.test/x64" || current.Author != "houko" ||
		!jsonEqual(current.Notes, []releaseNote{{"修复", "Win11 24H2 候选窗偏移"}, {"新增", "剪贴板历史支持固定条目"}}) {
		t.Fatalf("%+v", current)
	}
	if withdrawn := history.Releases[3]; withdrawn.Body != "### 说明\n- 缺少 vc_redist" || !jsonEqual(withdrawn.Notes, []releaseNote{{"说明", "缺少 vc_redist"}}) {
		t.Fatalf("withdrawal block leaked into the notes: %+v", withdrawn)
	}
	// Reads never ask for Actions access, so an installation without it still lists releases.
	f.mu.Lock()
	for _, body := range f.bodies["POST /app/installations/77/access_tokens"] {
		if perms, _ := body["permissions"].(map[string]any); perms["actions"] != nil {
			t.Errorf("a read requested %v", perms)
		}
	}
	f.mu.Unlock()
	// Reads are cached: the history reused the list's release read of the repository.
	if n := f.count("GET /repos/metasequoiaime/msime-windows/releases"); n != 1 {
		t.Fatal("release list read", n, "times")
	}

	hits := s.searchReleases("0.5.4")
	if len(hits) != 1 || hits[0] != (account.AdminSearchHit{Kind: "release", ID: "windows:windows-v0.5.4", Title: "Windows v0.5.4", Where: "发布管理", Target: "release"}) {
		t.Fatalf("%+v", hits)
	}
	if hits := s.searchReleases("WINDOWS"); len(hits) != 4 {
		t.Fatalf("%+v", hits)
	}
	if s.searchReleases(" ") != nil {
		t.Fatal("blank query matched")
	}

	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/api/releases/nope", 404, "not_found"},
		{"GET", "/api/releases/", 404, "not_found"},
		{"POST", "/api/releases", 405, "method_not_allowed"},
		{"DELETE", "/api/releases/windows", 405, "method_not_allowed"},
		{"GET", "/api/releases/windows/trigger", 405, "method_not_allowed"},
		{"GET", "/api/releases/windows/windows-v0.5.4/other", 404, "not_found"},
		{"POST", "/api/releases/windows/macos-v1.0.0/notes", 400, "invalid_tag"},
		{"POST", "/api/releases/windows/windows-v/withdraw", 400, "invalid_tag"},
		// The read-only role cannot write, and GitHub is never asked.
		{"POST", "/api/releases/windows/trigger", 403, "permission_denied"},
		{"POST", "/api/releases/windows/windows-v0.5.4/withdraw", 403, "permission_denied"},
		{"POST", "/api/releases/windows/windows-v0.5.4/notes", 403, "permission_denied"},
	} {
		if code := errorCode(t, s, adminRequest(s, tc.method, tc.path, "", readonly), tc.status); code != tc.code {
			t.Fatal(tc, code)
		}
	}
	if f.count("POST /repos/metasequoiaime/msime-windows/actions/workflows/release.yml/dispatches") != 0 || f.count("PATCH /repos/metasequoiaime/msime-windows/releases/3") != 0 {
		t.Fatal("a forbidden write reached GitHub")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestReleaseNoteParsing(t *testing.T) {
	body := "Intro line\n\n## 新增功能\n- [x] 四季皮肤\n1. 剪贴板\n<!-- hidden -->\n### Fixes\n- English fix\n### 待办 ###\n+ 上架材料\n```\ncode\n```\n---\n"
	got := parseReleaseNotes(body)
	want := []releaseNote{{"", "Intro line"}, {"新增", "四季皮肤"}, {"新增", "剪贴板"}, {"", "English fix"}, {"待办", "上架材料"}, {"待办", "code"}}
	if !jsonEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if len(parseReleaseNotes(strings.Repeat("- x\n", 300))) != maxReleaseNoteLines {
		t.Fatal("notes not bounded")
	}
	if n := parseReleaseNotes("- " + strings.Repeat("长", 600)); len([]rune(n[0].Text)) != 501 {
		t.Fatal("long line not cut")
	}
	if releaseUserBody(withdrawnBody("正文")) != "正文" || releaseUserBody(withdrawnBody("")) != "" || !releaseWithdrawn("\r\n"+withdrawnBody("x")) || releaseWithdrawn("正文 "+releaseWithdrawnMarker) {
		t.Fatal("withdrawal block round trip")
	}
	for _, tc := range []struct {
		status int
		header http.Header
		code   string
	}{
		{401, nil, "github_rejected"},
		{403, nil, "github_rejected"},
		{403, http.Header{"X-Ratelimit-Remaining": {"0"}}, "github_unavailable"},
		{403, http.Header{"Retry-After": {"60"}}, "github_unavailable"},
		{500, nil, "github_unavailable"},
	} {
		if code := releaseErrorCode(releaseStatusError(githubapp.Response{Status: tc.status, Header: tc.header})); code != tc.code {
			t.Fatal(tc, code)
		}
	}
	p := AdminPlatformConfig{TagPrefix: "ios-"}
	if releaseVersion(p, "ios-1.0.0") != "v1.0.0" || releaseVersion(p, "ios-v2") != "v2" || releaseVersion(p, "ios-beta") != "beta" {
		t.Fatal("version")
	}
	runs := func(states ...[2]string) ghCheckRuns {
		var r ghCheckRuns
		for _, s := range states {
			r.CheckRuns = append(r.CheckRuns, ghCheckRun{Name: "x", Status: s[0], Conclusion: s[1]})
		}
		return r
	}
	all := func(string) bool { return true }
	for _, tc := range []struct {
		runs ghCheckRuns
		want string
	}{
		{runs(), "unknown"},
		{runs([2]string{"completed", "success"}, [2]string{"completed", "skipped"}), "passed"},
		{runs([2]string{"completed", "success"}, [2]string{"queued", ""}), "pending"},
		{runs([2]string{"completed", "failure"}, [2]string{"queued", ""}), "failed"},
		{runs([2]string{"completed", "timed_out"}), "failed"},
	} {
		if state, _ := checkRunsState(tc.runs, all); state != tc.want {
			t.Fatal(tc, state)
		}
	}
}

func releaseDBServer(t *testing.T) (*Server, *fakeReleaseGitHub, *pgx.Conn, string) {
	t.Helper()
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("q", 48))
	s, err := New(Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin:   AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN", GitHub: AdminGitHubConfig{Platforms: releaseTestPlatforms}},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.CloseAccounts(); s.Close() })
	f := newFakeReleaseGitHub(t)
	s.adminGitHub = f.client(t)
	return s, f, admin, schema
}

func releaseWrite(path, body string) *http.Request {
	r := httptest.NewRequest("POST", "https://admin.example.com"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("q", 48))
	r.Header.Set("Content-Type", "application/json")
	return r
}

type auditRow struct {
	Action, Target string
	Detail         map[string]any
}

func releaseAudits(t *testing.T, admin *pgx.Conn, schema string) []auditRow {
	t.Helper()
	rows, err := admin.Query(context.Background(), `SELECT action,target,detail FROM `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` WHERE action LIKE 'release_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err = rows.Scan(&row.Action, &row.Target, &row.Detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// Every release write reaches GitHub first and is audited only after GitHub accepted it; a rejected write leaves no audit row.
func TestAdminReleaseWrites(t *testing.T) {
	s, f, admin, schema := releaseDBServer(t)
	dispatch := "POST /repos/metasequoiaime/msime-windows/actions/workflows/release.yml/dispatches"

	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/trigger", `{"version":"v0.5.5"}`), 200, nil)
	if body := f.lastBody(dispatch); !jsonEqual(body, map[string]any{"inputs": map[string]any{"version": "v0.5.5"}, "ref": "main"}) {
		t.Fatalf("%+v", body)
	}
	for _, tc := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/api/releases/linux/trigger", `{"version":"v1.0.0"}`, 409, "no_workflow"},
		{"/api/releases/windows/trigger", `{"version":"../main"}`, 400, "invalid_version"},
		{"/api/releases/windows/trigger", `{"version":"1.0","ref":"x"}`, 400, "invalid_json"},
		{"/api/releases/windows/windows-v9.9.9/notes", `{"body":"x"}`, 404, "not_found"},
		{"/api/releases/windows/windows-v0.5.4/notes", `{"body":"` + releaseWithdrawnMarker + `"}`, 400, "invalid_body"},
		{"/api/releases/windows/windows-v0.5.4/notes", `{"body":"` + strings.Repeat("x", maxReleaseBodyBytes+1) + `"}`, 400, "invalid_body"},
		{"/api/releases/windows/windows-v0.5.5/withdraw", `{}`, 409, "not_published"},
		{"/api/releases/windows/windows-v0.5.2/withdraw", `{}`, 409, "already_withdrawn"},
		{"/api/releases/ghost/ghost-v1/withdraw", `{}`, 502, "github_repo_not_found"},
	} {
		if code := errorCode(t, s, releaseWrite(tc.path, tc.body), tc.status); code != tc.code {
			t.Fatal(tc.path, tc.body[:min(len(tc.body), 40)], code)
		}
	}
	f.mu.Lock()
	f.status[dispatch] = 422
	f.mu.Unlock()
	if code := errorCode(t, s, releaseWrite("/api/releases/windows/trigger", `{"version":"v0.5.6"}`), 409); code != "workflow_rejected" {
		t.Fatal(code)
	}
	// A 403 is the App lacking Actions access, which no retry fixes.
	f.mu.Lock()
	f.status[dispatch] = 403
	f.mu.Unlock()
	if code := errorCode(t, s, releaseWrite("/api/releases/windows/trigger", `{"version":"v0.5.6"}`), 502); code != "github_rejected" {
		t.Fatal(code)
	}
	f.mu.Lock()
	f.status["PATCH /repos/metasequoiaime/msime-windows/releases/3"] = 500
	f.mu.Unlock()
	if code := errorCode(t, s, releaseWrite("/api/releases/windows/windows-v0.5.4/notes", `{"body":"x"}`), 502); code != "github_unavailable" {
		t.Fatal(code)
	}
	f.mu.Lock()
	delete(f.status, "PATCH /repos/metasequoiaime/msime-windows/releases/3")
	f.mu.Unlock()
	if audits := releaseAudits(t, admin, schema); len(audits) != 1 || audits[0].Action != "release_trigger" || audits[0].Target != "windows" || !jsonEqual(audits[0].Detail, map[string]any{"platform": "Windows", "version": "v0.5.5", "workflow": "release.yml"}) {
		t.Fatalf("%+v", audits)
	}

	// Editing notes replaces the body; a withdrawn release keeps its withdrawal block on top.
	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/windows-v0.5.4/notes", `{"body":"### 修复\r\n- 新说明\n"}`), 200, nil)
	if body := f.lastBody("PATCH /repos/metasequoiaime/msime-windows/releases/3"); !jsonEqual(body, map[string]any{"body": "### 修复\n- 新说明"}) {
		t.Fatalf("%+v", body)
	}
	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/windows-v0.5.2/notes", `{"body":"改发 v0.5.3"}`), 200, nil)
	if body := f.lastBody("PATCH /repos/metasequoiaime/msime-windows/releases/1"); body["body"] != withdrawnBody("改发 v0.5.3") {
		t.Fatalf("%+v", body)
	}

	// Withdrawing the repository's latest release turns it into a marked prerelease and makes the previous published release latest again.
	var result map[string]any
	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/windows-v0.5.4/withdraw", ``), 200, &result)
	if !jsonEqual(result, map[string]any{"latest_restored": true, "ok": true, "previous": "windows-v0.5.3", "was_latest": true}) {
		t.Fatalf("%+v", result)
	}
	if body := f.lastBody("PATCH /repos/metasequoiaime/msime-windows/releases/3"); body["prerelease"] != true || body["body"] != withdrawnBody("### 修复\n- 新说明") {
		t.Fatalf("%+v", body)
	}
	if body := f.lastBody("PATCH /repos/metasequoiaime/msime-windows/releases/2"); !jsonEqual(body, map[string]any{"make_latest": "true"}) {
		t.Fatalf("%+v", body)
	}
	// The write invalidated the cached reads, so the history shows the withdrawal at once.
	var history struct {
		Releases []releaseView `json:"releases"`
	}
	r := httptest.NewRequest("GET", "https://admin.example.com/api/releases/windows", nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("q", 48))
	serveAdminJSON(t, s, r, 200, &history)
	if history.Releases[1].Status != "withdrawn" || history.Releases[1].Body != "### 修复\n- 新说明" {
		t.Fatalf("%+v", history.Releases[1])
	}
	// GitHub refusing to make the previous release latest again does not undo the withdrawal; the result says the restore failed.
	f.mu.Lock()
	f.releases["metasequoiaime/msime-windows"][2]["prerelease"] = false
	f.releases["metasequoiaime/msime-windows"][2]["body"] = "### 修复\n- 新说明"
	f.latest["metasequoiaime/msime-windows"] = 3
	f.status["PATCH /repos/metasequoiaime/msime-windows/releases/2"] = 500
	f.mu.Unlock()
	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/windows-v0.5.4/withdraw", ``), 200, &result)
	if !jsonEqual(result, map[string]any{"latest_restored": false, "ok": true, "previous": "windows-v0.5.3", "was_latest": true}) {
		t.Fatalf("%+v", result)
	}
	f.mu.Lock()
	delete(f.status, "PATCH /repos/metasequoiaime/msime-windows/releases/2")
	f.latest["metasequoiaime/msime-windows"] = 9
	f.mu.Unlock()
	// Withdrawing a release that is not the repository's latest leaves latest alone: the only PATCH is the withdrawal itself.
	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/windows-v0.5.3/withdraw", ``), 200, &result)
	if !jsonEqual(result, map[string]any{"latest_restored": false, "ok": true, "previous": nil, "was_latest": false}) || f.count("PATCH /repos/metasequoiaime/msime-windows/releases/2") != 3 || f.latest["metasequoiaime/msime-windows"] != 9 {
		t.Fatalf("%+v", result)
	}

	audits := releaseAudits(t, admin, schema)
	var actions []string
	for _, a := range audits {
		actions = append(actions, a.Action+":"+a.Target)
	}
	if strings.Join(actions, ",") != "release_trigger:windows,release_notes:windows-v0.5.4,release_notes:windows-v0.5.2,release_withdraw:windows-v0.5.4,release_withdraw:windows-v0.5.4,release_withdraw:windows-v0.5.3" {
		t.Fatal(actions)
	}
	if !jsonEqual(audits[3].Detail, map[string]any{"latest_restored": true, "platform": "Windows", "previous": "windows-v0.5.3", "version": "v0.5.4", "was_latest": true}) {
		t.Fatalf("%+v", audits[3].Detail)
	}
	if !jsonEqual(audits[4].Detail, map[string]any{"latest_restored": false, "platform": "Windows", "previous": "windows-v0.5.3", "version": "v0.5.4", "was_latest": true}) {
		t.Fatalf("%+v", audits[4].Detail)
	}
	// Each accepted trigger and withdrawal reaches the console bell; rejected writes and note edits do not.
	if got := notificationRows(t, admin, schema); !slices.Equal(got, []string{"release release windows", "release release windows", "release release windows", "release release windows"}) {
		t.Fatal("release notifications", got)
	}

	// GitHub 的各种失败应答映射为后台的错误码；这些失败都不会改动发布、写审计或发通知。
	f.mu.Lock()
	f.releases["metasequoiaime/msime-windows"][2]["prerelease"] = false
	f.releases["metasequoiaime/msime-windows"][2]["body"] = "### 修复\n- 新说明"
	f.latest["metasequoiaime/msime-windows"] = 3
	delete(f.status, dispatch)
	f.mu.Unlock()
	const (
		repoRead  = "GET /repos/metasequoiaime/msime-windows"
		listRead  = "GET /repos/metasequoiaime/msime-windows/releases"
		latest    = "GET /repos/metasequoiaime/msime-windows/releases/latest"
		patch     = "PATCH /repos/metasequoiaime/msime-windows/releases/3"
		triggerTo = "/api/releases/windows/trigger"
		notesTo   = "/api/releases/windows/windows-v0.5.4/notes"
		withdraw  = "/api/releases/windows/windows-v0.5.4/withdraw"
	)
	for _, tc := range []struct {
		key        string
		forced     int
		path, body string
		status     int
		code       string
	}{
		{repoRead, 404, triggerTo, `{"version":"v0.5.7"}`, 502, "github_repo_not_found"},
		{repoRead, 401, triggerTo, `{"version":"v0.5.7"}`, 502, "github_rejected"},
		{repoRead, 500, triggerTo, `{"version":"v0.5.7"}`, 502, "github_unavailable"},
		{dispatch, 404, triggerTo, `{"version":"v0.5.7"}`, 409, "workflow_not_found"},
		{listRead, 500, notesTo, `{"body":"x"}`, 502, "github_unavailable"},
		{patch, 404, notesTo, `{"body":"x"}`, 404, "not_found"},
		{patch, 422, notesTo, `{"body":"x"}`, 409, "release_rejected"},
		{latest, 500, withdraw, ``, 502, "github_unavailable"},
		{patch, 422, withdraw, ``, 409, "release_rejected"},
	} {
		f.mu.Lock()
		f.status[tc.key] = tc.forced
		f.mu.Unlock()
		// 仓库信息的读取会缓存 ReadTTL，清掉缓存才能让每个用例都请求到假的 GitHub。
		s.adminGitHub.Invalidate("")
		code := errorCode(t, s, releaseWrite(tc.path, tc.body), tc.status)
		f.mu.Lock()
		delete(f.status, tc.key)
		f.mu.Unlock()
		if code != tc.code {
			t.Fatal(tc.key, tc.forced, tc.path, code)
		}
	}
	f.mu.Lock()
	still := f.releases["metasequoiaime/msime-windows"][2]["prerelease"]
	f.mu.Unlock()
	if still != false {
		t.Fatal("a failed withdrawal changed the release")
	}
	if n := len(releaseAudits(t, admin, schema)); n != len(audits) {
		t.Fatal("failed writes were audited", n)
	}
	if got := notificationRows(t, admin, schema); len(got) != 4 {
		t.Fatal("failed writes notified", got)
	}
}

// The daily job records every published asset's cumulative download count once per day; a second run the same day updates the counts instead of adding rows.
func TestReleaseAssetSnapshotJob(t *testing.T) {
	s, f, admin, schema := releaseDBServer(t)
	day := time.Date(2026, 10, 1, 23, 30, 0, 0, time.UTC)
	// The ghost platform cannot be read; the others are still recorded and the failure is reported.
	if err := s.snapshotReleaseAssets(context.Background(), s.adminGitHub, day); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.releases["metasequoiaime/msime-windows"][2]["assets"].([]any)[0].(map[string]any)["download_count"] = 130
	f.mu.Unlock()
	s.config.Admin.GitHub.Platforms = releaseTestPlatforms[:2]
	if err := s.snapshotReleaseAssets(context.Background(), s.adminGitHub, day); err != nil {
		t.Fatal(err)
	}
	rows, err := admin.Query(context.Background(), `SELECT tag||'/'||asset||'@'||to_char(day,'YYYY-MM-DD')||'='||download_count FROM `+pgx.Identifier{schema, "release_asset_snapshots"}.Sanitize()+` ORDER BY tag, asset`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	// The macos-v1.0.0 release shares the Windows repository but belongs to no configured platform, and the draft has no published assets.
	if strings.Join(got, ",") != "windows-v0.5.2/msime-windows-x64-setup.exe@2026-10-01=7,windows-v0.5.3/msime-windows-x64-setup.exe@2026-10-01=50,windows-v0.5.4/msime-windows-arm64-setup.exe@2026-10-01=20,windows-v0.5.4/msime-windows-x64-setup.exe@2026-10-01=130" {
		t.Fatal(got)
	}
	// Without a configured GitHub App the job returns at once; with one it runs until its context ends, so Close never waits on it. Each run gets its own Server copy, because the job New already started still reads s.config.
	for _, appID := range []int64{0, 42} {
		ctx, cancel := context.WithCancel(context.Background())
		job := &Server{config: s.config, accounts: s.accounts, adminGitHub: s.adminGitHub}
		job.config.Admin.GitHub.AppID = appID
		done := make(chan struct{})
		go func() { job.releaseSnapshotJob(ctx); close(done) }()
		if appID != 0 {
			cancel()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the job did not return", appID)
		}
		cancel()
	}
}

// The release search index belongs to its Server: one server's listing never shows up in another's search, and a new listing of a platform replaces the old one.
func TestReleaseSearchIndexIsPerServer(t *testing.T) {
	a, b := &Server{}, &Server{}
	for _, s := range []*Server{a, b} {
		s.config.Admin.GitHub.Platforms = releaseTestPlatforms
	}
	windows := releaseTestPlatforms[0]
	a.indexReleases(windows, []ghRelease{{TagName: "windows-v0.5.4"}, {TagName: "windows-v0.5.3"}})
	if hits := a.searchReleases("0.5.4"); len(hits) != 1 || hits[0].ID != "windows:windows-v0.5.4" || hits[0].Title != "Windows v0.5.4" || hits[0].Target != "release" {
		t.Fatalf("%+v", hits)
	}
	if hits := b.searchReleases("0.5"); len(hits) != 0 {
		t.Fatalf("another server's releases leaked: %+v", hits)
	}
	a.indexReleases(windows, []ghRelease{{TagName: "windows-v0.6.0"}})
	if hits := a.searchReleases("WINDOWS"); len(hits) != 1 || hits[0].ID != "windows:windows-v0.6.0" {
		t.Fatalf("%+v", hits)
	}
}
