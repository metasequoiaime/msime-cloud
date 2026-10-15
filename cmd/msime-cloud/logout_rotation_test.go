package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLogoutForgetsRotatedCredentialsOfSameLogin(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, all := range []bool{false, true} {
			name := "current"
			if legacy {
				name = "legacy"
			}
			if all {
				name += "/all"
			}
			t.Run(name, func(t *testing.T) {
				s := store{dir: t.TempDir()}
				now := time.Now()
				old := (tokenResponse{AccessToken: "synthetic-old-access", RefreshToken: "synthetic-old-refresh", ExpiresIn: 3600}).session(now)
				if legacy {
					old.LoginID = ""
				}
				rotated := (tokenResponse{AccessToken: "synthetic-rotated-access", RefreshToken: "synthetic-rotated-refresh", ExpiresIn: 3600}).session(now)
				rotated.LoginID = old.loginID()
				key := ""
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/logout" || r.Header.Get("Authorization") != "Bearer "+old.AccessToken {
						t.Errorf("退出请求没有使用原会话：%s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					// 服务端已验证原访问令牌，但退出响应返回前，另一进程保存了同一次登录的刷新结果。
					if err := s.put(key, &rotated); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer srv.Close()
				key = srv.URL
				if err := s.put(key, &old); err != nil {
					t.Fatal(err)
				}
				c := cli{client: srv.Client(), stdout: io.Discard, stderr: io.Discard, now: func() time.Time { return now }, env: func(name string) string {
					switch name {
					case "MSIME_CLOUD_CONFIG_DIR":
						return s.dir
					case "MSIME_CLOUD_URL":
						return key
					}
					return ""
				}}
				var args []string
				if all {
					args = []string{"--all"}
				}
				if err := c.logout(args); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.get(key); err != nil || ok {
					t.Fatalf("退出成功后仍保留同一次登录刷新的凭据：存在=%v，错误=%v", ok, err)
				}
			})
		}
	}
}
