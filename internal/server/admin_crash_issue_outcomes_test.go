package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

const crashOutcomeSignature = "0123456789abcdef"
const crashOutcomePath = "/api/crash-groups/" + crashOutcomeSignature + "/issue"
const crashOutcomeGitHubPath = "/repos/metasequoiaime/msime-ios/issues"

func crashOutcomeFixture(t *testing.T) (*Server, *fakeCrashGitHub, *pgx.Conn, string) {
	t.Helper()
	s, _, db, schema := releaseDBServer(t)
	if _, err := db.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+"(signature,platform,version,title) VALUES($1,'ios','1.0.0','合成崩溃')", crashOutcomeSignature); err != nil {
		t.Fatal(err)
	}
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "ios", Name: "iOS", Repo: "metasequoiaime/msime-ios", TagPrefix: "ios-v"}}
	f := &fakeCrashGitHub{}
	upstream := httptest.NewServer(f)
	t.Cleanup(upstream.Close)
	s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &githubapp.Cache{}}
	return s, f, db, schema
}

func crashOutcomeRequest(s *Server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, releaseWrite(crashOutcomePath, "{}"))
	return w
}

func TestCrashIssueUncertainOutcomeBlocksDuplicate(t *testing.T) {
	for _, outcome := range []string{"连接中断", "上游错误", "响应截断", "响应损坏", "缺少标识"} {
		t.Run(outcome, func(t *testing.T) {
			s, f, db, schema := crashOutcomeFixture(t)
			upstream := uncertainAdminWriteUpstream(t, f, "POST", crashOutcomeGitHubPath, outcome)
			s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			w := crashOutcomeRequest(s)
			if w.Code != 502 || !strings.Contains(w.Body.String(), `"github_uncertain"`) {
				t.Errorf("未知结果未提示核对 GitHub：%d %s", w.Code, w.Body.String())
			}
			var state string
			var attempts int
			if err := db.QueryRow(t.Context(), "SELECT status,(SELECT count(*) FROM "+pgx.Identifier{schema, "admin_audit"}.Sanitize()+" WHERE action='crash_group_issue_attempt' AND target=$1 AND actor='legacy-token') FROM "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" WHERE signature=$1", crashOutcomeSignature).Scan(&state, &attempts); err != nil || state != "known" || attempts != 1 {
				t.Errorf("未持久保存创建尝试：状态=%s 审计数=%d 错误=%v", state, attempts, err)
			}
			// 使用新服务和新连接池读取同一数据库，确认保护不会随请求或副本切换而丢失。
			replica, err := New(s.config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(replica.Close)
			t.Cleanup(replica.CloseAccounts)
			replica.adminGitHub = s.adminGitHub
			w = crashOutcomeRequest(replica)
			if w.Code != 409 || !strings.Contains(w.Body.String(), `"crash_group_not_open"`) || f.calls("POST "+crashOutcomeGitHubPath) != 1 {
				t.Errorf("另一个副本重复创建了 Issue：%d %s 写入次数=%d", w.Code, w.Body.String(), f.calls("POST "+crashOutcomeGitHubPath))
			}
		})
	}
}

func TestCrashIssueRecordFailureKeepsCreationClaim(t *testing.T) {
	s, f, db, schema := crashOutcomeFixture(t)
	if _, err := db.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{schema, "admin_audit"}.Sanitize()+" ADD CONSTRAINT reject_issue_record CHECK(action<>'crash_group_issue') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	w := crashOutcomeRequest(s)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"issue_not_recorded"`) || !strings.Contains(w.Body.String(), "https://github.com/metasequoiaime/msime-ios/issues/7") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = crashOutcomeRequest(s)
	if w.Code != 409 || f.calls("POST "+crashOutcomeGitHubPath) != 1 {
		t.Fatalf("登记失败后重复创建了 Issue：%d %s", w.Code, w.Body.String())
	}
}

func TestCrashIssueClaimAuditFailurePreventsWrite(t *testing.T) {
	s, f, db, schema := crashOutcomeFixture(t)
	if _, err := db.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{schema, "admin_audit"}.Sanitize()+" ADD CONSTRAINT reject_issue_attempt CHECK(action<>'crash_group_issue_attempt') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	w := crashOutcomeRequest(s)
	var state string
	if err := db.QueryRow(t.Context(), "SELECT status FROM "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" WHERE signature=$1", crashOutcomeSignature).Scan(&state); err != nil || state != "open" || w.Code != 503 || f.calls("POST "+crashOutcomeGitHubPath) != 0 {
		t.Fatalf("未能记录尝试时仍写入 GitHub 或更改状态：%d %s 状态=%s 错误=%v", w.Code, w.Body.String(), state, err)
	}
}

func TestCrashIssueOnlyCreatesForOpenGroups(t *testing.T) {
	for _, state := range []string{"known", "fixed"} {
		t.Run(state, func(t *testing.T) {
			s, f, db, schema := crashOutcomeFixture(t)
			if _, err := db.Exec(t.Context(), "UPDATE "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" SET status=$1", state); err != nil {
				t.Fatal(err)
			}
			w := crashOutcomeRequest(s)
			if w.Code != 409 || !strings.Contains(w.Body.String(), `"crash_group_not_open"`) || f.calls("POST "+crashOutcomeGitHubPath) != 0 {
				t.Fatalf("非未处理分组仍创建 Issue：%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCrashIssueRejectAuditFailureKeepsClaim(t *testing.T) {
	s, f, db, schema := crashOutcomeFixture(t)
	f.status = 422
	if _, err := db.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{schema, "admin_audit"}.Sanitize()+" ADD CONSTRAINT reject_issue_rejection CHECK(action<>'crash_group_issue_rejected') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	w := crashOutcomeRequest(s)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = crashOutcomeRequest(s)
	if w.Code != 409 || f.calls("POST "+crashOutcomeGitHubPath) != 1 {
		t.Fatalf("未能审计恢复状态时仍重复写入：%d %s", w.Code, w.Body.String())
	}
}

func TestCrashIssueRejectionPreservesConcurrentStatus(t *testing.T) {
	s, f, db, schema := crashOutcomeFixture(t)
	f.status = 422
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == crashOutcomeGitHubPath {
			if _, err := db.Exec(t.Context(), "UPDATE "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" SET status='fixed' WHERE signature=$1", crashOutcomeSignature); err != nil {
				t.Error(err)
			}
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
	w := crashOutcomeRequest(s)
	var state string
	if err := db.QueryRow(t.Context(), "SELECT status FROM "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" WHERE signature=$1", crashOutcomeSignature).Scan(&state); err != nil || state != "fixed" || w.Code != 502 || !strings.Contains(w.Body.String(), `"github_rejected"`) {
		t.Fatalf("明确拒绝的恢复覆盖了并发状态：%d %s 状态=%s 错误=%v", w.Code, w.Body.String(), state, err)
	}
}

func TestCrashIssueSuccessPreservesConcurrentStatus(t *testing.T) {
	for _, state := range []string{"open", "fixed"} {
		t.Run(state, func(t *testing.T) {
			s, f, db, schema := crashOutcomeFixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && r.URL.Path == crashOutcomeGitHubPath {
					if _, err := db.Exec(t.Context(), "UPDATE "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" SET status=$2 WHERE signature=$1", crashOutcomeSignature, state); err != nil {
						t.Error(err)
					}
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			w := crashOutcomeRequest(s)
			var got string
			if err := db.QueryRow(t.Context(), "SELECT status FROM "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+" WHERE signature=$1", crashOutcomeSignature).Scan(&got); err != nil || got != state || w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"`+state+`"`) {
				t.Fatalf("成功登记或响应覆盖了并发状态：%d %s 状态=%s 错误=%v", w.Code, w.Body.String(), got, err)
			}
		})
	}
}
