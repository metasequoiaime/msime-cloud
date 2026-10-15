package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleaseNotesAndWithdrawalAcrossReplicas(t *testing.T) {
	for _, firstAction := range []string{"notes", "withdraw"} {
		t.Run(firstAction, func(t *testing.T) {
			first, f, _, _ := releaseDBServer(t)
			// 两个平台指向同一仓库，锁不能仅按平台 ID 划分。
			alias := releaseTestPlatforms[0]
			alias.ID = "windows-alias"
			first.config.Admin.GitHub.Platforms = append(append([]AdminPlatformConfig(nil), releaseTestPlatforms...), alias)
			second, err := New(first.config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { second.CloseAccounts(); second.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			entered := make(chan struct{})
			resume := make(chan struct{})
			var resumeOnce sync.Once
			unblock := func() { resumeOnce.Do(func() { close(resume) }) }
			defer unblock()
			var blocked atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PATCH" && r.URL.Path == "/repos/metasequoiaime/msime-windows/releases/3" && blocked.CompareAndSwap(false, true) {
					// 在第一个请求已经读到旧正文后，暂停它向 GitHub 写入。
					close(entered)
					select {
					case <-resume:
					case <-r.Context().Done():
						return
					}
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			for _, s := range []*Server{first, second} {
				s.adminGitHub = f.client(t)
				s.adminGitHub.APIURL = upstream.URL
				s.adminGitHub.HTTP = upstream.Client()
			}
			const notes = `{"body":"新的发布说明"}`
			firstBody, secondBody, secondAction := notes, "", "withdraw"
			if firstAction == "withdraw" {
				firstBody, secondBody, secondAction = "", notes, "notes"
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				first.ServeHTTP(w, releaseWrite("/api/releases/windows/windows-v0.5.4/"+firstAction, firstBody).WithContext(ctx))
				done <- w
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("第一个请求未到达 GitHub 写入阶段", ctx.Err())
			}
			secondPath := "/api/releases/windows-alias/windows-v0.5.4/" + secondAction
			readsBefore := f.count("GET /repos/metasequoiaime/msime-windows/releases")
			concurrent := httptest.NewRecorder()
			second.ServeHTTP(concurrent, releaseWrite(secondPath, secondBody).WithContext(ctx))
			if concurrent.Code == 503 {
				if reads := f.count("GET /repos/metasequoiaime/msime-windows/releases"); reads != readsBefore {
					t.Fatal("未取得锁的请求读取了 GitHub 发布状态")
				}
			}
			unblock()
			select {
			case w := <-done:
				if w.Code != 200 {
					t.Fatalf("第一个请求失败：%d %s", w.Code, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("第一个请求未完成", ctx.Err())
			}
			if concurrent.Code == 503 {
				serveAdminJSON(t, second, releaseWrite(secondPath, secondBody).WithContext(ctx), 200, nil)
			} else if concurrent.Code != 200 {
				t.Fatalf("并发请求返回意外状态：%d %s", concurrent.Code, concurrent.Body.String())
			}
			f.mu.Lock()
			body := f.releases[alias.Repo][2]["body"]
			prerelease := f.releases[alias.Repo][2]["prerelease"]
			f.mu.Unlock()
			if body != withdrawnBody("新的发布说明") || prerelease != true {
				t.Fatalf("并发写入丢失撤回状态或新说明：body=%q prerelease=%v", body, prerelease)
			}
			if concurrent.Code != 503 || concurrent.Header().Get("Retry-After") == "" {
				t.Fatalf("同仓库并发写入应拒绝并允许稍后重试：%d %s", concurrent.Code, concurrent.Body.String())
			}
		})
	}
}

func TestReleaseWriteLockUnavailable(t *testing.T) {
	s, f, _, _ := releaseDBServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	unlock, ok := s.lockReleaseWrite(w, ctx, releaseTestPlatforms[0])
	problem, _ := decodeBody(t, w)["error"].(map[string]any)
	if ok || unlock != nil || w.Code != 503 || problem["code"] != "auth_unavailable" || problem["message"] != "auth_unavailable" {
		t.Fatalf("数据库不可用时应返回统一错误：ok=%v status=%d body=%s", ok, w.Code, w.Body.String())
	}
	if f.count("GET /repos/metasequoiaime/msime-windows/releases") != 0 {
		t.Fatal("数据库未能加锁时访问了 GitHub")
	}
}
