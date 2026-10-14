package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// The personal page end to end against PostgreSQL: a Google session mints a personal access token, the token then authenticates as its holder with the holder's role, and losing membership revokes it.
func TestAdminPersonalTokenEndToEnd(t *testing.T) {
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_ADMIN_GOOGLE_SECRET", "google-secret")
	s, err := New(Config{
		Auth:  account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin: AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN_UNSET", Google: AdminGoogleConfig{ClientID: "client", SecretEnv: "TEST_ADMIN_GOOGLE_SECRET", AllowedEmails: []string{"owner@example.com"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	ctx := context.Background()
	call := func(method, path, body, cookie, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://admin.example.com"+path, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: cookie})
			if method == "POST" {
				r.Header.Set("Origin", "https://admin.example.com")
			}
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	decode := func(w *httptest.ResponseRecorder, status int) map[string]any {
		t.Helper()
		if w.Code != status {
			t.Fatal(w.Code, w.Body.String())
		}
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	owner, err := s.accounts.CreateAdminSession(ctx, account.AdminIdentity{Subject: "o", Email: "owner@example.com", Name: "Owner", UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15"})
	if err != nil {
		t.Fatal(err)
	}
	me := decode(call("GET", "/api/me", "", owner, ""), 200)
	sessions := me["sessions"].([]any)
	if me["name"] != "Owner" || me["owner"] != true || me["via"] != "session" || len(sessions) != 1 || sessions[0].(map[string]any)["current"] != true || sessions[0].(map[string]any)["device"] != "macOS · Safari" {
		t.Fatal(me)
	}
	// Cookie writes keep their Origin check.
	r := httptest.NewRequest("POST", "https://admin.example.com/api/me", strings.NewReader(`{"action":"regenerate_token"}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: owner})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cookie write without Origin accepted", w.Code)
	}
	ownerToken := decode(call("POST", "/api/me", `{"action":"regenerate_token"}`, owner, ""), 200)["token"].(string)
	if me = decode(call("GET", "/api/me", "", "", ownerToken), 200); me["via"] != "token" || me["email"] != "owner@example.com" {
		t.Fatal(me)
	}
	if w = call("POST", "/api/me", `{"action":"regenerate_token"}`, "", ownerToken); w.Code != 403 {
		t.Fatal("token renewed itself", w.Code)
	}
	// 命令行的有效 Bearer 会话也能签发，来源验证必须沿用其鉴权优先级，不能误用附带的无效 Cookie。
	ownerToken = decode(call("POST", "/api/me", `{"action":"regenerate_token"}`, strings.Repeat("0", 64), owner), 200)["token"].(string)

	// An owner adds a read-only member, who mints a token that carries the readonly role.
	decode(call("POST", "/api/admins", `{"action":"add","email":"helper@example.com","role":"readonly"}`, "", ownerToken), 200)
	helper, err := s.accounts.CreateAdminSession(ctx, account.AdminIdentity{Subject: "h", Email: "helper@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	helperToken := decode(call("POST", "/api/me", `{"action":"regenerate_token"}`, helper, ""), 200)["token"].(string)
	if me = decode(call("GET", "/api/me", "", "", helperToken), 200); me["role"] != "readonly" {
		t.Fatal(me)
	}
	if w = call("POST", "/api/permissions", `{"action":"grant","role":"readonly","permission":"ban_users"}`, "", helperToken); w.Code != 403 {
		t.Fatal("readonly token changed permissions", w.Code)
	}
	if w = call("POST", "/api/admins", `{"action":"set_role","email":"helper@example.com","role":"operator"}`, "", helperToken); w.Code != 403 {
		t.Fatal("member managed admins", w.Code)
	}
	perms := decode(call("GET", "/api/permissions", "", "", helperToken), 200)
	if len(perms["members"].([]any)) != 2 {
		t.Fatal(perms)
	}
	// The owner promotes the member; the token picks the new role up on its next request.
	decode(call("POST", "/api/admins", `{"action":"set_role","email":"helper@example.com","role":"maintainer"}`, owner, ""), 200)
	decode(call("POST", "/api/permissions", `{"action":"grant","role":"readonly","permission":"ban_users"}`, "", helperToken), 200)
	decode(call("POST", "/api/permissions", `{"action":"revoke","role":"readonly","permission":"ban_users"}`, owner, ""), 200)
	if w = call("POST", "/api/permissions", `{"action":"revoke","role":"maintainer","permission":"manage_permissions"}`, owner, ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	var actors []string
	rows, err := admin.Query(ctx, `SELECT actor FROM `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` WHERE action LIKE 'permission\_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var actor string
		if err = rows.Scan(&actor); err != nil {
			t.Fatal(err)
		}
		actors = append(actors, actor)
	}
	rows.Close()
	if strings.Join(actors, ",") != "pat:helper@example.com,google:o:owner@example.com" {
		t.Fatal(actors)
	}
	// Disabling the member revokes the token at once.
	decode(call("POST", "/api/admins", `{"action":"disable","email":"helper@example.com"}`, owner, ""), 200)
	if w = call("GET", "/api/me", "", "", helperToken); w.Code != 401 {
		t.Fatal("disabled member token accepted", w.Code)
	}
	if w = call("GET", "/api/me", "", "", ownerToken[:len(ownerToken)-1]+"x"); w.Code != 401 {
		t.Fatal("forged token accepted", w.Code)
	}
}
