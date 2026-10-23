package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func issueDetailReadFixture(t *testing.T) (*Server, *fakeIssuesGitHub) {
	t.Helper()
	f := newFakeIssuesGitHub(t)
	f.add(issuesWinRepo, &fakeIssue{number: 10, title: "合成问题", body: "合成正文", author: "tester", state: "open", created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	s := &Server{
		config:      Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{IssueRepos: []string{issuesWinRepo}}}},
		adminGitHub: f.client(),
	}
	return s, f
}

func startIssueDetailRead(ctx context.Context, s *Server) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/issues/"+issuesWinRepo+"/10", nil).WithContext(ctx)
		s.issueDetail(w, r, issuesWinRepo+"/10")
		done <- w
	}()
	return done
}

func TestIssueDetailReadOverlap(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		t.Run(strconv.FormatBool(paginated), func(t *testing.T) {
			s, f := issueDetailReadFixture(t)
			if paginated {
				issue := f.get(issuesWinRepo, 10)
				for i := range 150 {
					issue.timeline = append(issue.timeline, map[string]any{"event": "commented", "body": "合成评论 " + strconv.Itoa(i), "created_at": issue.created.UTC().Format(time.RFC3339)})
				}
				f.add(issuesWinRepo, &issue)
			}
			entered, release := make(chan string, 2), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			f.setBefore(func(r *http.Request) {
				if r.URL.Path != "/repos/"+issuesWinRepo+"/issues/10" && r.URL.Path != "/repos/"+issuesWinRepo+"/issues/10/timeline" || r.URL.Query().Get("page") != "" {
					return
				}
				entered <- r.URL.Path
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := startIssueDetailRead(ctx, s)
			paths := map[string]bool{}
			for range 2 {
				select {
				case path := <-entered:
					paths[path] = true
				case <-ctx.Done():
					t.Fatal("issue 与时间线未同时进入上游：", paths)
				}
			}
			if len(paths) != 2 {
				t.Fatal("未并行读取两个独立路径：", paths)
			}
			unblock()
			select {
			case w := <-done:
				var body struct {
					Issue struct {
						Number int    `json:"number"`
						Body   string `json:"body"`
					} `json:"issue"`
					Timeline  []issueEvent `json:"timeline"`
					Truncated bool         `json:"timeline_truncated"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.Issue.Number != 10 || body.Issue.Body != "合成正文" || body.Truncated != paginated {
					t.Fatalf("并行读取的详情结果错误：%d %s，解码=%v", w.Code, w.Body.String(), err)
				}
				if !paginated && len(body.Timeline) != 1 || paginated && (len(body.Timeline) != 51 || body.Timeline[1].Text != "合成评论 100" || body.Timeline[50].Text != "合成评论 149") {
					t.Fatal("时间线创建事件或末页选择变化：", body.Timeline)
				}
			case <-ctx.Done():
				t.Fatal("上游放行后详情读取未完成")
			}
		})
	}
}

func TestIssueDetailReadFailurePriority(t *testing.T) {
	for _, sample := range []struct {
		name                        string
		issueStatus, timelineStatus int
		malformed                   string
		status                      int
		code                        string
	}{
		{"issue_missing", 404, 403, "", 404, "not_found"},
		{"issue_refused", 403, 404, "", 502, "github_rejected"},
		{"issue_unavailable", 500, 404, "", 502, "github_unavailable"},
		{"timeline_missing", 200, 404, "", 404, "not_found"},
		{"timeline_refused", 200, 403, "", 502, "github_rejected"},
		{"timeline_unavailable", 200, 500, "", 502, "github_unavailable"},
		{"issue_malformed", 200, 404, "issue", 502, "github_unavailable"},
		{"timeline_malformed", 200, 200, "timeline", 502, "github_unavailable"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			s, f := issueDetailReadFixture(t)
			issuePath := "/repos/" + issuesWinRepo + "/issues/10"
			f.status["GET "+issuePath] = sample.issueStatus
			f.status["GET "+issuePath+"/timeline"] = sample.timelineStatus
			// 成功状态交回原模拟接口生成正文；失败状态由覆盖表返回。
			if sample.issueStatus == 200 {
				delete(f.status, "GET "+issuePath)
			}
			if sample.timelineStatus == 200 {
				delete(f.status, "GET "+issuePath+"/timeline")
			}
			var listing atomic.Int32
			f.setBefore(func(r *http.Request) {
				if issueReadKind(r) != "" {
					listing.Add(1)
				}
			})
			if sample.malformed != "" {
				path := issuePath
				if sample.malformed == "timeline" {
					path += "/timeline"
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == path {
						_, _ = io.WriteString(w, "{")
						return
					}
					f.serve(w, r)
				}))
				t.Cleanup(upstream.Close)
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			select {
			case w := <-startIssueDetailRead(ctx, s):
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != sample.status || issueErrorCode(body) != sample.code || listing.Load() != 0 {
					t.Fatalf("错误优先级变化或错误后仍读取相似问题：%d %s，列表请求=%d", w.Code, w.Body.String(), listing.Load())
				}
			case <-ctx.Done():
				t.Fatal("错误响应未完成")
			}
		})
	}
}

func TestIssueDetailReadCancelsTimelineWait(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(strconv.FormatBool(cached), func(t *testing.T) {
			s, f := issueDetailReadFixture(t)
			if !cached {
				s.adminGitHub.Cache = nil
			}
			issuePath := "/repos/" + issuesWinRepo + "/issues/10"
			f.status["GET "+issuePath] = 404
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			f.setBefore(func(r *http.Request) {
				switch r.URL.Path {
				case issuePath:
					select {
					case <-entered:
					case <-r.Context().Done():
					}
				case issuePath + "/timeline":
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			select {
			case w := <-startIssueDetailRead(ctx, s):
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 404 || issueErrorCode(body) != "not_found" || ctx.Err() != nil {
					t.Fatalf("issue 失败未及时取消时间线等待：%d %s，超时=%v", w.Code, w.Body.String(), ctx.Err())
				}
			case <-ctx.Done():
				t.Fatal("issue 失败后仍等待被阻塞的时间线")
			}
		})
	}
}

func TestIssueDetailReadRequestCancellation(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(strconv.FormatBool(cached), func(t *testing.T) {
			s, f := issueDetailReadFixture(t)
			if !cached {
				s.adminGitHub.Cache = nil
			}
			entered, release := make(chan struct{}, 2), make(chan struct{})
			defer close(release)
			f.setBefore(func(r *http.Request) {
				if r.URL.Path != "/repos/"+issuesWinRepo+"/issues/10" && r.URL.Path != "/repos/"+issuesWinRepo+"/issues/10/timeline" {
					return
				}
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(deadline)
			defer cancel()
			done := startIssueDetailRead(ctx, s)
			for range 2 {
				select {
				case <-entered:
				case <-deadline.Done():
					t.Fatal("取消测试未进入两项读取")
				}
			}
			cancel()
			select {
			case w := <-done:
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 502 || issueErrorCode(body) != "github_unavailable" || deadline.Err() != nil {
					t.Fatalf("调用方取消后的错误或等待行为变化：%d %s", w.Code, w.Body.String())
				}
			case <-deadline.Done():
				t.Fatal("调用方取消后并行读取未退出")
			}
		})
	}
}

func BenchmarkIssueDetailColdReads(b *testing.B) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		b.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/77/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repos/" + issuesWinRepo + "/issues/10", "/repos/" + issuesWinRepo + "/issues/10/timeline":
			// 这项基准刻意模拟两项独立读取各需 20ms 的上游延迟。
			time.Sleep(20 * time.Millisecond)
			if r.URL.Path == "/repos/"+issuesWinRepo+"/issues/10" {
				_, _ = io.WriteString(w, `{"number":10,"title":"合成问题","state":"open"}`)
			} else {
				_, _ = io.WriteString(w, "[]")
			}
		default:
			_, _ = io.WriteString(w, "[]")
		}
	}))
	b.Cleanup(upstream.Close)
	gh := &githubapp.Client{AppID: 42, InstallationID: 77, Key: key, APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &githubapp.Cache{}}
	s := &Server{config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{IssueRepos: []string{issuesWinRepo}}}}, adminGitHub: gh}
	if _, err := gh.Token(b.Context(), issuesWinRepo, issueTokenPerms); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		gh.Invalidate("/repos/")
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/issues/"+issuesWinRepo+"/10", nil)
		s.issueDetail(w, r, issuesWinRepo+"/10")
		if w.Code != 200 {
			b.Fatalf("详情读取失败：%d %s", w.Code, w.Body.String())
		}
	}
}
