package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPendingUserRequestDoesNotAdoptAnotherLogin(t *testing.T) {
	for _, action := range []string{"write", "logout", "logout all"} {
		for _, phase := range []string{"expired", "unauthorized"} {
			for _, sameUser := range []bool{false, true} {
				name := action + "/" + phase + "/different user"
				if sameUser {
					name = action + "/" + phase + "/same user"
				}
				for _, legacy := range []bool{false, true} {
					format := "current"
					if legacy {
						format = "legacy"
					}
					t.Run(name+"/"+format, func(t *testing.T) {
						s := store{dir: t.TempDir()}
						now := time.Now()
						// 两种凭据格式都不能改用后来登录的会话。
						old := session{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: now.Add(time.Hour), User: []byte(`{"id":"old-user"}`)}
						if !legacy {
							old = (tokenResponse{AccessToken: old.AccessToken, RefreshToken: old.RefreshToken, ExpiresIn: 3600, User: old.User}).session(now)
						}
						if phase == "expired" {
							old.ExpiresAt = now.Add(-time.Minute)
						}
						user := []byte(`{"id":"new-user"}`)
						if sameUser {
							user = old.User
						}
						fresh := (tokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600, User: user}).session(now)
						key := ""
						var replaced sync.Once
						replace := func() {
							replaced.Do(func() {
								if err := s.put(key, &fresh); err != nil {
									t.Error(err)
								}
							})
						}
						requests := make(chan string, 3)
						srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							authorization := r.Header.Get("Authorization")
							requests <- authorization
							if phase == "unauthorized" && authorization == "Bearer "+old.AccessToken {
								replace()
								w.WriteHeader(http.StatusUnauthorized)
								return
							}
							w.Header().Set("Content-Type", "application/json")
							io.WriteString(w, `{"ok":true}`)
						}))
						defer srv.Close()
						key = srv.URL
						if err := s.put(key, &old); err != nil {
							t.Fatal(err)
						}
						c := cli{client: srv.Client(), stdout: io.Discard, stderr: io.Discard, env: func(name string) string {
							switch name {
							case "MSIME_CLOUD_CONFIG_DIR":
								return s.dir
							case "MSIME_CLOUD_URL":
								return key
							}
							return ""
						}, now: func() time.Time {
							// 在旧会话已读出后完成另一次登录，再触发刷新。
							if phase == "expired" {
								replace()
							}
							return now
						}}
						var err error
						switch action {
						case "write":
							var response *http.Response
							response, err = c.send(request{method: "PUT", path: "/v1/users/me/preferences", body: []byte(`{"revision":0,"settings":{}}`), contentType: "application/json"})
							if response != nil {
								response.Body.Close()
							}
						case "logout":
							err = c.logout(nil)
						case "logout all":
							err = c.logout([]string{"--all"})
						}
						if err == nil {
							t.Error("会话已换成另一次登录，旧命令仍报告成功")
						}
						close(requests)
						for authorization := range requests {
							if strings.Contains(authorization, fresh.AccessToken) {
								t.Error("旧请求使用了另一次登录的凭据")
							}
						}
						got, ok, err := s.get(key)
						if err != nil || !ok || got.AccessToken != fresh.AccessToken || got.RefreshToken != fresh.RefreshToken {
							t.Fatalf("旧命令改变了新登录：存在=%v，会话=%+v，错误=%v", ok, got, err)
						}
					})
				}
			}
		}
	}
}

func TestConcurrentRefreshKeepsOneLoginAcrossRotations(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current credentials"
		if legacy {
			name = "legacy credentials"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.signIn()
			c := cli{env: func(name string) string { return h.env[name] }, client: http.DefaultClient, now: time.Now}
			original, ok, err := c.store().get(h.url)
			if err != nil || !ok {
				t.Fatalf("没有读到初始登录：%v", err)
			}
			if legacy {
				// 用旧格式重写，模拟升级前已经保存的登录。
				original = session{AccessToken: original.AccessToken, RefreshToken: original.RefreshToken, ExpiresAt: original.ExpiresAt, User: original.User}
				if err := c.store().put(h.url, &original); err != nil {
					t.Fatal(err)
				}
			}
			latest := original
			for rotation := 0; rotation < 3; rotation++ {
				latest, err = c.refresh(h.url, latest)
				if err != nil {
					t.Fatal(err)
				}
			}
			// 较早的请求跨过多次轮换后仍应复用同一次登录，不能重放刷新令牌。
			got, err := c.refresh(h.url, original)
			if err != nil || got.AccessToken != latest.AccessToken || h.cloud.refreshes != 3 {
				t.Fatalf("旧请求未复用最新会话：错误=%v，刷新次数=%d", err, h.cloud.refreshes)
			}
			if code, _, errText := h.run("", "whoami"); code != 0 {
				t.Fatalf("轮换后请求失败：%d %s", code, errText)
			}
		})
	}
}
