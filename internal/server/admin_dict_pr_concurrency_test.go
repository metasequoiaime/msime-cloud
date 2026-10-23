package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

var dictionaryReviewBodies = map[string]string{
	"approve": `{"head_sha":"head12"}`,
	"reject":  `{"reason":"合成拒绝理由","head_sha":"head12"}`,
	"trim":    `{"keep":[0,4,6],"head_sha":"head12"}`,
}

func dictionaryReviewRequest(ctx context.Context, action, body string) *http.Request {
	r := httptest.NewRequest("POST", "https://admin.example.com/api/dict-prs/12/"+action, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+dictTestAdminToken)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func dictionaryReviewReplica(t *testing.T, config Config) *Server {
	t.Helper()
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.CloseAccounts(); second.Close() })
	return second
}

func TestDictPRReviewWhileWebsiteSubmission(t *testing.T) {
	for _, action := range []string{"approve", "reject", "trim"} {
		t.Run(action, func(t *testing.T) {
			s, f, _, _ := dictPRServer(t, "")
			second := dictionaryReviewReplica(t, s.config)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(resume) })
			defer unblock()
			var calls atomic.Int32
			var blocked atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				root := "/repos/" + dictTestRepo
				if r.Method == "PUT" && r.URL.Path == root+"/contents/custom/translations.txt" && blocked.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
						return
					}
				}
				// 将官网投稿的列表和默认分支读取映射到审核测试的合成仓库。
				if r.Method == "GET" && r.URL.Path == root+"/pulls" {
					f.mu.Lock()
					f.reply(w, 200, []map[string]any{f.pullJSON(f.pull(12))})
					f.mu.Unlock()
					return
				}
				if r.Method == "GET" && strings.HasPrefix(r.URL.Path, root+"/contents/") && r.URL.Query().Get("ref") == "main" {
					r = r.Clone(r.Context())
					query := r.URL.Query()
					query.Set("ref", "base1")
					r.URL.RawQuery = query.Encode()
				}
				f.serve(w, r)
			}))
			t.Cleanup(upstream.Close)
			s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			ws := newWordSubmitter(WordSubmissionsConfig{GitHub: WordsGitHubConfig{AppID: 42, InstallationID: 77, Repository: dictTestRepo, Branch: "main", APIURL: upstream.URL, key: s.adminGitHub.Key}}, nil, nil)
			ws.client, ws.locks = upstream.Client(), second.accounts
			done := make(chan error, 1)
			go func() {
				_, err := ws.submit(ctx, submission{kind: kindTranslations, lines: translationLines([]translationSubmissionEntry{{"未审核新词", "unreviewed word"}})})
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("官网投稿未进入写入阶段", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			before := calls.Load()
			w := httptest.NewRecorder()
			s.ServeHTTP(w, dictionaryReviewRequest(ctx, action, dictionaryReviewBodies[action]))
			if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"server_busy"`) || w.Header().Get("Retry-After") != "1" || calls.Load() != before {
				t.Errorf("官网投稿持锁时审核仍访问 GitHub，或未返回可重试错误：%d %s calls=%d/%d", w.Code, w.Body.String(), calls.Load(), before)
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("官网投稿失败", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			f.mu.Lock()
			p := *f.pull(12)
			translations := f.commits[p.head]["custom/translations.txt"]
			f.mu.Unlock()
			if p.state != "open" || p.head == "head12" || !strings.Contains(translations, "未审核新词") {
				t.Errorf("并发审核关闭或覆盖了官网投稿：%+v translations=%q", p, translations)
			}
			w = httptest.NewRecorder()
			s.ServeHTTP(w, dictionaryReviewRequest(ctx, action, dictionaryReviewBodies[action]))
			if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"pr_changed"`) {
				t.Errorf("投稿完成后仍允许使用旧 head 审核：%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDictPRReviewAcrossReplicas(t *testing.T) {
	for _, action := range []string{"approve", "reject", "trim"} {
		t.Run(action, func(t *testing.T) {
			first, f, _, _ := dictPRServer(t, "")
			// GitHub 仓库名不区分大小写，两个副本必须使用同一把锁。
			config := first.config
			config.Admin.GitHub.DictionaryRepo = strings.ToUpper(dictTestRepo)
			second := dictionaryReviewReplica(t, config)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(resume) })
			defer unblock()
			var blocked atomic.Bool
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == "GET" && r.URL.Path == "/repos/"+dictTestRepo+"/pulls/12" && blocked.CompareAndSwap(false, true) {
					// 在第一次读取 PR 时暂停，要求锁覆盖整个读改写过程。
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
						return
					}
				}
				f.serve(w, r)
			}))
			t.Cleanup(upstream.Close)
			for _, s := range []*Server{first, second} {
				s.adminGitHub = dictGitHubClient(t, f)
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				first.ServeHTTP(w, dictionaryReviewRequest(ctx, action, dictionaryReviewBodies[action]))
				done <- w
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("审核未开始读取 PR", ctx.Err())
			}
			before := calls.Load()
			for _, competing := range []string{"approve", "reject", "trim"} {
				w := httptest.NewRecorder()
				second.ServeHTTP(w, dictionaryReviewRequest(ctx, competing, dictionaryReviewBodies[competing]))
				if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"server_busy"`) || w.Header().Get("Retry-After") != "1" || calls.Load() != before {
					t.Errorf("另一副本的 %s 未在读取 GitHub 前拒绝：%d %s calls=%d/%d", competing, w.Code, w.Body.String(), calls.Load(), before)
				}
			}
			unlock, err := second.accounts.WaitLock(ctx, "word-submissions:"+dictTestRepo, 0)
			if unlock != nil {
				unlock()
			}
			if !errors.Is(err, account.ErrLockBusy) {
				t.Errorf("审核没有持有官网投稿使用的仓库锁：%v", err)
			}
			unblock()
			select {
			case w := <-done:
				if w.Code != 200 {
					t.Fatalf("持锁审核未成功：%d %s", w.Code, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			unlock, err = second.accounts.WaitLock(ctx, "word-submissions:"+dictTestRepo, 0)
			if err != nil {
				t.Fatal("审核结束后未释放锁", err)
			}
			unlock()
		})
	}
}

func TestDictPRReviewLockUnavailable(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, action := range []string{"approve", "reject", "trim"} {
			t.Run(strconv.FormatBool(closed)+"/"+action, func(t *testing.T) {
				s, f, _, _ := dictPRServer(t, "")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if closed {
					s.CloseAccounts()
				} else {
					cancel()
				}
				w := httptest.NewRecorder()
				r := dictionaryReviewRequest(ctx, action, dictionaryReviewBodies[action])
				// 直接调用写处理函数，避免鉴权层先拦截数据库或取消错误。
				switch action {
				case "approve":
					s.approveDictPR(w, r, s.adminGitHub, 12)
				case "reject":
					s.rejectDictPR(w, r, s.adminGitHub, 12)
				case "trim":
					s.trimDictPR(w, r, s.adminGitHub, 12)
				}
				f.mu.Lock()
				calls := slices.Clone(f.calls)
				f.mu.Unlock()
				if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"auth_unavailable"`) || len(calls) != 0 {
					t.Errorf("未取得数据库锁时仍访问 GitHub，或错误格式不统一：%d %s calls=%v", w.Code, w.Body.String(), calls)
				}
			})
		}
	}
}

func TestDictPRReviewAuditsWithOneFreeConnection(t *testing.T) {
	for _, tc := range []struct {
		name, action, failurePath string
		failureStatus, status     int
		want                      []string
	}{
		{"approve", "approve", "", 0, 200, []string{"dict_pr_trim", "dict_pr_approve"}},
		{"reject", "reject", "", 0, 200, []string{"dict_pr_reject"}},
		{"trim", "trim", "", 0, 200, []string{"dict_pr_trim"}},
		{"merge_conflict", "approve", "/pulls/12/merge", 409, 409, []string{"dict_pr_trim"}},
		{"partial_trim_before_approve", "approve", "/contents/custom/translations.txt", 500, 502, []string{"dict_pr_trim"}},
		{"partial_trim", "trim", "/contents/custom/translations.txt", 500, 502, []string{"dict_pr_trim"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, conn, schema := dictPRServer(t, "")
			if tc.failurePath != "" {
				f.status["PUT /repos/"+dictTestRepo+tc.failurePath] = tc.failureStatus
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			// 服务池固定八个连接，只留一个供审核锁和后续审计依次使用。
			for i := range 7 {
				unlock, err := s.accounts.WaitLock(ctx, t.Name()+":"+strconv.Itoa(i), 0)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			body := dictionaryReviewBodies[tc.action]
			if tc.action == "approve" {
				body = dictionaryReviewBodies["trim"]
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, dictionaryReviewRequest(ctx, tc.action, body))
			if w.Code != tc.status {
				t.Fatalf("只剩一个池连接时审核失败：%d %s", w.Code, w.Body.String())
			}
			var got []string
			for _, audit := range dictAudit(t, conn, schema) {
				got = append(got, audit.action)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("审核占用最后连接导致审计丢失，或精简和通过的顺序改变：got=%v want=%v", got, tc.want)
			}
			unlock, err := s.accounts.WaitLock(ctx, "word-submissions:"+dictTestRepo, 0)
			if err != nil {
				t.Fatal("审核结束后未释放锁或池连接", err)
			}
			unlock()
		})
	}
}
