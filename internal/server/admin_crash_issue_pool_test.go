package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestConcurrentCrashIssuesDoNotExhaustPool(t *testing.T) {
	s, f, db, schema := releaseDBServer(t)
	groupTable := pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()
	auditTable := pgx.Identifier{schema, "admin_audit"}.Sanitize()
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "ios", Name: "iOS", Repo: "synthetic/ios", TagPrefix: "ios-v"}}
	const requests = 8
	for i := range requests {
		if _, err := db.Exec(t.Context(), `INSERT INTO `+groupTable+`(signature,platform,version,title) VALUES($1,'ios','1.0','合成崩溃')`, fmt.Sprintf("%016x", i)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{}, requests)
	resume := make(chan struct{})
	var resumeOnce sync.Once
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	defer unblock()
	var issueNumber atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/repos/synthetic/ios/issues" {
			f.ServeHTTP(w, r)
			return
		}
		entered <- struct{}{}
		// 等待所有请求都持有分组锁，保证写回时连接池已经被这些锁占满。
		select {
		case <-resume:
		case <-r.Context().Done():
			return
		}
		number := issueNumber.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": number, "html_url": fmt.Sprintf("https://example.test/issues/%d", number)})
	}))
	t.Cleanup(upstream.Close)
	s.adminGitHub = f.client(t)
	s.adminGitHub.APIURL = upstream.URL
	s.adminGitHub.HTTP = upstream.Client()
	done := make(chan *httptest.ResponseRecorder, requests)
	for i := range requests {
		go func() {
			w := httptest.NewRecorder()
			r := releaseWrite(fmt.Sprintf("/api/crash-groups/%016x/issue", i), `{}`).WithContext(ctx)
			s.ServeHTTP(w, r)
			done <- w
		}()
	}
	for i := range requests {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatalf("仅 %d 个请求在持锁后到达 GitHub，发生连接池等待：%v", i, ctx.Err())
		}
	}
	unblock()
	for range requests {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatalf("连接池满时 Issue 创建或登记失败：%d %s", w.Code, w.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("Issue 写回未完成", ctx.Err())
		}
	}
	var groups, audits int
	if err := db.QueryRow(t.Context(), `SELECT
(SELECT count(*) FROM `+groupTable+` WHERE status='known' AND issue_url IS NOT NULL),
(SELECT count(*) FROM `+auditTable+` WHERE action='crash_group_issue')`).Scan(&groups, &audits); err != nil || groups != requests || audits != requests {
		t.Fatalf("创建成功的 Issue 未全部登记和审计：groups=%d audits=%d err=%v", groups, audits, err)
	}
}
