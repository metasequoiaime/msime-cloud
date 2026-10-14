package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminLogoutPreservesNewerBrowserCookies(t *testing.T) {
	db, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_ADMIN_GOOGLE_SECRET", "google-secret")
	t.Setenv("TEST_ADMIN_LOGOUT_TOKEN", strings.Repeat("a", 40))
	s, err := New(Config{
		Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin: AdminConfig{Enabled: true, Host: "admin.example.test", TokenEnv: "TEST_ADMIN_LOGOUT_TOKEN", Google: AdminGoogleConfig{
			ClientID: "client", SecretEnv: "TEST_ADMIN_GOOGLE_SECRET", AllowedEmails: []string{"owner@example.test"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	target, err := url.Parse("https://admin.example.test/api/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	createSession := func() string {
		t.Helper()
		token, err := s.accounts.CreateAdminSession(t.Context(), account.AdminIdentity{Subject: "owner", Email: "owner@example.test"})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	call := func(method, path, body, bearer string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://admin.example.test"+path, strings.NewReader(body))
		r.Header.Set("Origin", "https://admin.example.test")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		return w
	}
	for _, kind := range []string{"cookie", "bearer", "bearer_other_cookie", "pat", "legacy", "anonymous"} {
		t.Run(kind, func(t *testing.T) {
			old := createSession()
			browserOld := old
			if kind == "bearer_other_cookie" {
				browserOld = createSession()
			}
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			cookies := httptest.NewRecorder()
			s.adminCookie(cookies, browserOld, false, 8*60*60)
			jar.SetCookies(target, cookies.Result().Cookies())
			var bearer string
			requestCookies := jar.Cookies(target)
			switch kind {
			case "bearer", "bearer_other_cookie":
				bearer = old
			case "pat":
				issued := call("POST", "/api/me", `{"action":"regenerate_token"}`, "", requestCookies)
				var token struct {
					Token string `json:"token"`
				}
				if err := json.Unmarshal(issued.Body.Bytes(), &token); err != nil || token.Token == "" {
					t.Fatal("个人访问令牌签发失败", err)
				}
				bearer = token.Token
			case "legacy":
				bearer = s.config.Admin.token
			case "anonymous":
				requestCookies = nil
			}
			logout := call("POST", "/api/auth/logout", "", bearer, requestCookies)
			want := `"authenticated":false`
			if kind == "anonymous" {
				want = `"authenticated":true`
			}
			w := call("GET", "/api/auth/session", "", "", jar.Cookies(target))
			if !strings.Contains(w.Body.String(), want) {
				t.Fatal("退出未按来源凭据隔离会话", w.Body.String())
			}
			if kind == "bearer_other_cookie" {
				w = call("GET", "/api/auth/session", "", bearer, nil)
				if !strings.Contains(w.Body.String(), `"authenticated":false`) {
					t.Fatal("退出未撤销 Bearer 来源会话", w.Body.String())
				}
			}
			if kind == "pat" || kind == "legacy" {
				w = call("GET", "/api/auth/session", "", bearer, nil)
				if !strings.Contains(w.Body.String(), `"authenticated":true`) {
					t.Fatal("退出不应撤销非会话令牌", w.Body.String())
				}
			}
			// 模拟另一个标签页先收到新登录及授权流程 Cookie，再收到迟到的退出响应。
			fresh, flow := createSession(), adminRandom()
			cookies = httptest.NewRecorder()
			s.adminCookie(cookies, fresh, false, 8*60*60)
			s.adminCookie(cookies, flow, true, 600)
			jar.SetCookies(target, cookies.Result().Cookies())
			jar.SetCookies(target, logout.Result().Cookies())
			w = call("GET", "/api/auth/session", "", "", jar.Cookies(target))
			if !strings.Contains(w.Body.String(), `"authenticated":true`) {
				t.Error("迟到的退出响应清掉了新登录", w.Body.String())
			}
			var gotFlow string
			for _, cookie := range jar.Cookies(target) {
				if cookie.Name == s.adminCookieName(true) {
					gotFlow = cookie.Value
				}
			}
			if gotFlow != flow {
				t.Error("迟到的退出响应清掉了新的授权流程")
			}
		})
	}
	t.Run("撤销失败保持两个旧会话", func(t *testing.T) {
		bearer, cookie := createSession(), createSession()
		function := pgx.Identifier{schema, "reject_logout_cookie"}.Sanitize()
		table := pgx.Identifier{schema, "admin_sessions"}.Sanitize()
		// 仅拒绝 Cookie 会话的删除；分两次删除的实现会先撤销 Bearer，单条语句必须完整回滚。
		sql := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.token_hash='%x' THEN RAISE EXCEPTION 'private logout failure'; END IF;
 RETURN OLD;
END $$`, function, sha256.Sum256([]byte(cookie)))
		if _, err := db.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(t.Context(), `CREATE TRIGGER reject_logout BEFORE DELETE ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+function+`() `); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "https://admin.example.test/api/auth/logout", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: cookie})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private") || len(w.Result().Cookies()) != 0 {
			t.Fatal("数据库失败未按预期返回", w.Code, w.Body.String())
		}
		for _, token := range []string{bearer, cookie} {
			w = call("GET", "/api/auth/session", "", token, nil)
			if !strings.Contains(w.Body.String(), `"authenticated":true`) {
				t.Fatal("失败的退出只撤销了部分会话", w.Body.String())
			}
		}
	})
}
