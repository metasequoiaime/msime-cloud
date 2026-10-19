package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func issueActionReply(t *testing.T, s *Server, action string, n int) map[string]any {
	t.Helper()
	status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":%q,"items":[{"repo":%q,"n":%d}]}`, action, issuesWinRepo, n))
	if status != 200 {
		t.Fatal(status, reply)
	}
	return reply
}

func TestAdminIssueUndoRejectsRepeatedRequest(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	undo := issueUndoBody(t, issueActionReply(t, s, "close", 10))
	if status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", undo); status != 200 {
		t.Fatal(status, reply)
	}
	writes, audits := len(f.writeLog()), len(issueAudits(t, conn, schema))
	if status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", undo); status != 409 || issueErrorCode(reply) != "conflict" {
		t.Fatal("重复撤销未被拒绝", status, reply)
	}
	if len(f.writeLog()) != writes || len(issueAudits(t, conn, schema)) != audits {
		t.Fatal("重复撤销写入 GitHub 或成功审计")
	}
}

func TestAdminIssueUndoBypassesCachedTimeline(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		t.Run(strconv.FormatBool(paginated), func(t *testing.T) {
			s, f, conn, schema := issuesFixture(t)
			if paginated {
				f.mu.Lock()
				for range 105 {
					f.event(f.issues[issuesWinRepo][10], "commented")
				}
				f.mu.Unlock()
			}
			undo := issueUndoBody(t, issueActionReply(t, s, "close", 10))
			path := "/repos/" + issuesWinRepo + "/issues/10/timeline?per_page=100"
			token, err := s.adminGitHub.Token(t.Context(), issuesWinRepo, issueTokenPerms)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := s.adminGitHub.Get(t.Context(), token, path)
			if err != nil || !resp.OK() {
				t.Fatal(resp.Status, err)
			}
			if paginated {
				resp, err = s.adminGitHub.Get(t.Context(), token, path+"&page=2")
				if err != nil || !resp.OK() {
					t.Fatal(resp.Status, err)
				}
			}
			// 模拟 GitHub 网页或旧副本的同秒重新打开、再关闭；最终快照和 updated_at 均不变。
			f.mu.Lock()
			issue := f.issues[issuesWinRepo][10]
			f.event(issue, "reopened")
			f.event(issue, "closed")
			f.mu.Unlock()
			writes, audits := len(f.writeLog()), len(issueAudits(t, conn, schema))
			status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", undo)
			if status != 409 || issueErrorCode(reply) != "conflict" || len(f.writeLog()) != writes || len(issueAudits(t, conn, schema)) != audits {
				t.Fatal("时间线缓存漏过外部状态循环", status, reply)
			}
		})
	}
}

func TestAdminIssueUndoSnapshotReadFailureKeepsSuccess(t *testing.T) {
	for _, failRead := range []string{"issue", "timeline", "last_page"} {
		t.Run(failRead, func(t *testing.T) {
			s, f, conn, schema := issuesFixture(t)
			if failRead == "last_page" {
				f.mu.Lock()
				for range 100 {
					f.event(f.issues[issuesWinRepo][10], "commented")
				}
				f.mu.Unlock()
			}
			var reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isIssue := r.Method == "GET" && r.URL.Path == "/repos/"+issuesWinRepo+"/issues/10"
				if isIssue && failRead == "issue" && reads.Add(1) == 2 || r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/timeline") && (failRead == "timeline" || failRead == "last_page" && r.URL.Query().Get("page") == "2") {
					w.WriteHeader(503)
					return
				}
				f.serve(w, r)
			}))
			t.Cleanup(upstream.Close)
			s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			reply := issueActionReply(t, s, "close", 10)
			if reply["undo"] != nil || len(f.writeLog()) != 1 || f.get(issuesWinRepo, 10).state != "closed" || len(issueAudits(t, conn, schema)) != 1 {
				t.Fatal("补读失败诱发重复写入或丢失成功结果", reply, f.writeLog())
			}
		})
	}
}

func TestAdminIssueUndoVersionReadFailureDoesNotWrite(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	undo := issueUndoBody(t, issueActionReply(t, s, "close", 10))
	f.mu.Lock()
	f.status["GET /repos/"+issuesWinRepo+"/issues/10/timeline"] = 503
	f.mu.Unlock()
	writes, audits := len(f.writeLog()), len(issueAudits(t, conn, schema))
	status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", undo)
	if status != 502 || issueErrorCode(reply) != "github_unavailable" || len(f.writeLog()) != writes || len(issueAudits(t, conn, schema)) != audits {
		t.Fatal("无法校验版本时仍执行撤销", status, reply)
	}
}

func TestAdminIssueUndoVersionValidation(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	for _, tc := range []struct{ action, version string }{
		{"close", strings.Repeat("a", 64)}, {"triage", strings.Repeat("a", 64)},
		{"reopen", "short"}, {"untriage", strings.Repeat("g", 64)},
	} {
		status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":%q,"items":[{"repo":%q,"n":10,"version":%q}]}`, tc.action, issuesWinRepo, tc.version))
		if status != 400 || issueErrorCode(reply) != "invalid_items" {
			t.Fatal(tc, status, reply)
		}
	}
	if len(f.writeLog()) != 0 || len(f.tokens) != 0 {
		t.Fatal("非法版本访问了 GitHub")
	}
}

func TestIssueVersionRejectsInvalidTimeline(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{}`, `[null]`, `[{"event":"closed","created_at":"2026-10-20T00:00:00Z"}]`, `[{"id":1,"event":"closed"}]`, `[{"id":1,"event":"closed","created_at":"bad"}]`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			t.Cleanup(upstream.Close)
			f := newFakeIssuesGitHub(t)
			gh := f.client()
			gh.APIURL, gh.HTTP = upstream.URL, upstream.Client()
			version, err := new(Server).issueVersion(context.Background(), gh, "synthetic-token", issueRef{Repo: issuesWinRepo, N: 10}, issueGitHub{ID: 1, State: "closed", CreatedAt: time.Now()})
			if err == nil || version != "" {
				t.Fatal("无效时间线生成了可撤销版本", version, err)
			}
		})
	}
}

func TestAdminIssueUndoBatchPreservesChangedItems(t *testing.T) {
	s, f, conn, schema := issuesFixture(t)
	status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":"close","items":[{"repo":%q,"n":10},{"repo":%q,"n":11}]}`, issuesWinRepo, issuesWinRepo))
	if status != 200 {
		t.Fatal(status, reply)
	}
	undo := issueUndoBody(t, reply)
	issueActionReply(t, s, "reopen", 10)
	issueActionReply(t, s, "close", 10)
	audits := len(issueAudits(t, conn, schema))
	status, reply = issuesRequest(t, s, "POST", "/api/issues/actions", undo)
	failed, _ := reply["failed"].([]any)
	if status != 200 || reply["affected"] != 1.0 || len(failed) != 1 || failed[0].(map[string]any)["code"] != "conflict" || failed[0].(map[string]any)["n"] != 10.0 {
		t.Fatal(status, reply)
	}
	if f.get(issuesWinRepo, 10).state != "closed" || f.get(issuesWinRepo, 11).state != "open" || len(issueAudits(t, conn, schema)) != audits+1 {
		t.Fatal("批量撤销覆盖了已变更条目或漏记成功项")
	}
}

func TestAdminIssueVersionCaptureHoldsSharedLock(t *testing.T) {
	first, f, _, _ := issuesFixture(t)
	second, err := New(first.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.CloseAccounts(); second.Close() })
	second.adminGitHub = f.client()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered, resume := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	defer unblock()
	var block atomic.Bool
	var calls atomic.Int32
	f.setBefore(func(r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/timeline") && block.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		}
	})
	type result struct {
		status int
		reply  map[string]any
	}
	done := make(chan result, 1)
	go func() {
		status, reply := issuesRequest(t, first, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":"close","items":[{"repo":%q,"n":10}]}`, issuesWinRepo))
		done <- result{status, reply}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("未进入版本捕获", ctx.Err())
	}
	before := calls.Load()
	status, reply := issuesRequest(t, second, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":"reopen","items":[{"repo":%q,"n":10}]}`, issuesWinRepo))
	if status != 503 || issueErrorCode(reply) != "server_busy" || calls.Load() != before {
		t.Error("版本捕获前释放了跨副本锁", status, reply)
	}
	unblock()
	select {
	case got := <-done:
		if got.status != 200 || got.reply["undo"] == nil {
			t.Fatal(got)
		}
		// 另一副本已处理状态循环后，原副本的旧撤销必须读取共享 GitHub 的最新版本。
		issueActionReply(t, second, "reopen", 10)
		issueActionReply(t, second, "close", 10)
		if status, reply := issuesRequest(t, first, "POST", "/api/issues/actions", issueUndoBody(t, got.reply)); status != 409 || issueErrorCode(reply) != "conflict" {
			t.Fatal("旧版本跨副本仍可撤销", status, reply)
		}
	case <-ctx.Done():
		t.Fatal("版本捕获未结束", ctx.Err())
	}
}

func TestIssueVersionIgnoresLabelAndAssigneeOrder(t *testing.T) {
	f := newFakeIssuesGitHub(t)
	f.add(issuesWinRepo, &fakeIssue{number: 10, state: "open", created: time.Now(), labels: []string{"a", "b"}, assignees: []string{"alice", "bob"}, timeline: []map[string]any{{"event": "labeled", "created_at": "2026-10-20T00:00:00Z"}}})
	gh, ref := f.client(), issueRef{Repo: issuesWinRepo, N: 10}
	read := func() string {
		t.Helper()
		issue, err := issueFetch(t.Context(), gh, "ghs_synthetic", ref, false)
		if err != nil {
			t.Fatal(err)
		}
		version, err := new(Server).issueVersion(t.Context(), gh, "ghs_synthetic", ref, issue)
		if err != nil {
			t.Fatal(err)
		}
		return version
	}
	before := read()
	f.mu.Lock()
	f.issues[issuesWinRepo][10].labels = []string{"b", "a"}
	f.issues[issuesWinRepo][10].assignees = []string{"bob", "alice"}
	f.mu.Unlock()
	if read() != before {
		t.Fatal("GitHub 返回顺序改变使撤销失效")
	}
	f.mu.Lock()
	f.issues[issuesWinRepo][10].assignees = []string{"carol"}
	f.mu.Unlock()
	if read() == before {
		t.Fatal("负责人改变未使撤销失效")
	}
}

func TestAdminIssueUndoRejectsChangedPlatformAssignee(t *testing.T) {
	first, f, _, _ := issuesFixture(t)
	f.mu.Lock()
	f.issues[issuesWinRepo][10].assignees = []string{"fanlusky"}
	f.mu.Unlock()
	undo := issueUndoBody(t, issueActionReply(t, first, "triage", 10))
	config := first.config
	config.Admin.GitHub.Platforms = slices.Clone(config.Admin.GitHub.Platforms)
	config.Admin.GitHub.Platforms[0].Assignee = "fanlusky"
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.CloseAccounts(); second.Close() })
	second.adminGitHub = f.client()
	writes := len(f.writeLog())
	status, reply := issuesRequest(t, second, "POST", "/api/issues/actions", undo)
	if status != 409 || issueErrorCode(reply) != "conflict" || len(f.writeLog()) != writes || !slices.Contains(f.get(issuesWinRepo, 10).assignees, "fanlusky") {
		t.Fatal("负责人配置变化后撤销删除了原有指派", status, reply)
	}
}

func issueUndoBody(t *testing.T, reply map[string]any) string {
	t.Helper()
	if reply["undo"] == nil {
		t.Fatal("缺少已确认操作的撤销", reply)
	}
	body, err := json.Marshal(reply["undo"])
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestAdminIssueUndoRejectsLaterStateCycle(t *testing.T) {
	for _, action := range []string{"close", "mark_dup", "triage"} {
		t.Run(action, func(t *testing.T) {
			s, f, conn, schema := issuesFixture(t)
			undo := issueUndoBody(t, issueActionReply(t, s, action, 10))
			reverse := "reopen"
			if action == "triage" {
				reverse = "untriage"
			}
			issueActionReply(t, s, reverse, 10)
			issueActionReply(t, s, action, 10)
			writes, audits := len(f.writeLog()), len(issueAudits(t, conn, schema))
			status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", undo)
			if status != 409 || issueErrorCode(reply) != "conflict" {
				t.Fatalf("旧撤销覆盖后续操作：%d %v", status, reply)
			}
			if len(f.writeLog()) != writes || len(issueAudits(t, conn, schema)) != audits {
				t.Fatal("冲突仍写入 GitHub 或成功审计")
			}
		})
	}
}
