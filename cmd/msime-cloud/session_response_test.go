package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// 登录或刷新遇到不完整的成功响应时，不能用不可续期或已经过期的凭据覆盖原会话。
func TestInvalidUserSessionResponsesPreserveCredentials(t *testing.T) {
	for _, operation := range []string{"signin", "refresh"} {
		for _, test := range []struct{ name, body string }{
			{"missing_access", `{"refresh_token":"new-refresh","expires_in":900}`},
			{"missing_refresh", `{"access_token":"new-access","expires_in":900}`},
			{"missing_expiry", `{"access_token":"new-access","refresh_token":"new-refresh"}`},
			{"zero_expiry", `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":0}`},
			{"negative_expiry", `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":-1}`},
			{"overflow_expiry", `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":18446744075}`},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				h := newHarness(t)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, test.body)
				}))
				t.Cleanup(upstream.Close)
				h.env["MSIME_CLOUD_URL"] = upstream.URL
				c := cli{env: func(name string) string { return h.env[name] }, stdout: io.Discard, stderr: io.Discard, client: upstream.Client(), now: func() time.Time { return h.now }}
				kept := session{AccessToken: "kept-access", RefreshToken: "kept-refresh", ExpiresAt: h.now.Add(time.Hour)}
				if err := c.store().put(upstream.URL, &kept); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(c.store().path())
				if err != nil {
					t.Fatal(err)
				}
				if operation == "signin" {
					err = c.signIn(upstream.URL, "challenge", "code")
				} else {
					_, err = c.refresh(upstream.URL, kept)
				}
				if err == nil {
					t.Error("无效会话响应仍报告成功")
				}
				after, err := os.ReadFile(c.store().path())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Error("无效会话响应覆盖了原凭据文件")
				}
			})
		}
	}
}

// 管理员登录也必须先确认有效期，不能保存一个无法使用的成功响应。
func TestInvalidAdminSessionResponsesPreserveCredentials(t *testing.T) {
	for _, body := range []string{
		`{"expires_in":28800}`,
		`{"token":"new-admin"}`,
		`{"token":"new-admin","expires_in":0}`,
		`{"token":"new-admin","expires_in":-1}`,
		`{"token":"new-admin","expires_in":18446744075}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := newHarness(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/auth/cli/finish" {
					_, _ = io.WriteString(w, body)
					return
				}
				h.cloud.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			h.env["MSIME_ADMIN_URL"] = upstream.URL
			credentials := store{dir: h.dir}
			kept := session{AccessToken: "kept-admin", ExpiresAt: h.now.Add(time.Hour)}
			if err := credentials.put(adminKey(upstream.URL), &kept); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(credentials.path())
			if err != nil {
				t.Fatal(err)
			}
			if code, _, _ := h.runWithBrowser(browser(t, url.Values{"code": {"admin-code"}}), "login", "admin"); code != 1 {
				t.Errorf("无效管理员会话仍登录成功：退出码=%d", code)
			}
			after, err := os.ReadFile(credentials.path())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("无效管理员会话覆盖了原凭据文件")
			}
		})
	}
}
