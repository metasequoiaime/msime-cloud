package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIssueActionRollbackAcrossReplicas(t *testing.T) {
	for _, action := range []string{"triage", "mark_dup"} {
		t.Run(action, func(t *testing.T) {
			first, f, _, _ := issuesFixture(t)
			second, err := New(first.config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { second.CloseAccounts(); second.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(resume) })
			defer unblock()
			method, path := "POST", "/repos/"+issuesWinRepo+"/issues/10/assignees"
			label := issueTriagedLabel
			if action == "mark_dup" {
				method, path, label = "PATCH", "/repos/"+issuesWinRepo+"/issues/10", issueDuplicateLabel
			}
			var blocked atomic.Bool
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == method && r.URL.Path == path && blocked.CompareAndSwap(false, true) {
					// 第一条请求已加标签，暂停它的后续步骤，等待另一副本尝试同一操作。
					close(entered)
					select {
					case <-resume:
						w.WriteHeader(422)
					case <-r.Context().Done():
					}
					return
				}
				f.serve(w, r)
			}))
			t.Cleanup(upstream.Close)
			for _, s := range []*Server{first, second} {
				s.adminGitHub = f.client()
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			}
			request := func(s *Server, repo string, number int) *httptest.ResponseRecorder {
				body := fmt.Sprintf(`{"action":%q,"items":[{"repo":%q,"n":%d}]}`, action, repo, number)
				r := httptest.NewRequest("POST", "https://admin.example.com/api/issues/actions", strings.NewReader(body)).WithContext(ctx)
				r.Header.Set("Authorization", "Bearer "+issuesTestAdminToken)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request(first, issuesWinRepo, 10) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("第一条请求未进入后续写入", ctx.Err())
			}
			callsBefore := calls.Load()
			concurrent := request(second, strings.ToUpper(issuesWinRepo), 10)
			if concurrent.Code == 503 && calls.Load() != callsBefore {
				t.Fatal("未取得锁的请求仍访问 GitHub")
			}
			if other := request(second, issuesWinRepo, 11); other.Code != 200 {
				t.Fatalf("同仓库其他 Issue 被错误阻塞：%d %s", other.Code, other.Body.String())
			}
			unblock()
			select {
			case w := <-done:
				if w.Code != 502 || !strings.Contains(w.Body.String(), "github_rejected") {
					t.Fatalf("第一条请求未按拒绝处理：%d %s", w.Code, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("第一条请求未结束", ctx.Err())
			}
			if concurrent.Code == 503 {
				if retry := request(second, issuesWinRepo, 10); retry.Code != 200 {
					t.Fatalf("锁释放后不能重试：%d %s", retry.Code, retry.Body.String())
				}
			} else if concurrent.Code != 200 {
				t.Fatalf("并发请求返回意外状态：%d %s", concurrent.Code, concurrent.Body.String())
			}
			got := f.get(issuesWinRepo, 10)
			if !slices.Contains(got.labels, label) || action == "triage" && !slices.Contains(got.assignees, "houko") || action == "mark_dup" && got.state != "closed" {
				t.Fatalf("失败请求的回滚覆盖了另一副本成功的操作：%+v", got)
			}
			if concurrent.Code != 503 || concurrent.Header().Get("Retry-After") == "" || !strings.Contains(concurrent.Body.String(), "server_busy") {
				t.Fatalf("同一 Issue 并发写入应拒绝并允许稍后重试：%d %s", concurrent.Code, concurrent.Body.String())
			}
		})
	}
}

func TestIssueActionLockUnavailable(t *testing.T) {
	s, f, _, _ := issuesFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	detail, _, err := s.issueAction(ctx, s.adminGitHub, "close", issueRef{Repo: issuesWinRepo, N: 10}, "")
	var failure *issueError
	if detail != nil || !errors.As(err, &failure) || failure.status != 503 || failure.code != "auth_unavailable" {
		t.Fatalf("无法加锁时应在访问 GitHub 前返回数据库错误：detail=%v err=%v", detail, err)
	}
	if len(f.writeLog()) != 0 {
		t.Fatal("无法加锁时向 GitHub 写入")
	}
}
