package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUserCallStillPrefersEnvironmentToken(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.env["MSIME_CLOUD_TOKEN"] = "synthetic-device-token"
	if code, _, _ := h.run("", "call", "GET", "/v1/users/me"); code != 1 {
		t.Fatalf("合成服务应拒绝设备令牌：退出码=%d", code)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if got := h.cloud.seen[len(h.cloud.seen)-1]; got != "GET /v1/users/me auth=Bearer synthetic-device-token" {
		t.Fatalf("通用请求没有优先使用环境令牌：%s", got)
	}
	if h.cloud.refreshes != 0 {
		t.Fatal("环境令牌被拒绝后不应刷新本地会话")
	}
	if _, ok, err := (store{dir: h.dir}).get(h.url); err != nil || !ok {
		t.Fatalf("通用请求清理了本地会话：存在=%v，错误=%v", ok, err)
	}
}

func TestLogoutUsesKeptSessionWithEnvironmentToken(t *testing.T) {
	for _, all := range []bool{false, true} {
		for _, state := range []string{"active", "expired", "rejected access"} {
			name := state
			if all {
				name += " all"
			}
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				type logoutRequest struct {
					authorization string
					all           bool
				}
				requests := make(chan logoutRequest, 2)
				const externalToken = "other-user-access"
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/auth/logout" {
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						var body struct {
							All bool `json:"all"`
						}
						if err := json.Unmarshal(data, &body); err != nil {
							t.Error(err)
						}
						requests <- logoutRequest{r.Header.Get("Authorization"), body.All}
						r.Body = io.NopCloser(bytes.NewReader(data))
					}
					// 环境令牌属于另一个有效账户，其退出也会成功。
					if r.Header.Get("Authorization") == "Bearer "+externalToken {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					h.cloud.ServeHTTP(w, r)
				}))
				defer srv.Close()
				h.env["MSIME_CLOUD_URL"] = srv.URL
				h.signIn()
				h.env["MSIME_CLOUD_TOKEN"] = externalToken
				if state == "expired" {
					h.now = h.now.Add(time.Hour)
				} else if state == "rejected access" {
					h.cloud.mu.Lock()
					h.cloud.access = "different-access"
					h.cloud.mu.Unlock()
				}
				args := []string{"logout"}
				if all {
					args = append(args, "--all")
				}
				if code, _, errText := h.run("", args...); code != 0 {
					t.Fatalf("退出失败：%d %s", code, errText)
				}
				close(requests)
				for got := range requests {
					if got.authorization == "Bearer "+externalToken || got.authorization == "" || got.all != all {
						t.Errorf("退出使用了错误凭据或范围：%+v，all=%v", got, all)
					}
				}
				if _, ok, err := (store{dir: h.dir}).get(srv.URL); err != nil || ok {
					t.Errorf("退出后仍保留本地会话：存在=%v，错误=%v", ok, err)
				}
				h.cloud.mu.Lock()
				defer h.cloud.mu.Unlock()
				wantRefreshes := 0
				if state != "active" {
					wantRefreshes = 1
				}
				if h.cloud.refreshes != wantRefreshes {
					t.Errorf("刷新次数=%d，期望=%d", h.cloud.refreshes, wantRefreshes)
				}
				if h.env["MSIME_CLOUD_TOKEN"] != externalToken {
					t.Fatal("退出修改了环境令牌")
				}
			})
		}
	}
}
