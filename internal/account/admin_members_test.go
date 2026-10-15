package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminMemberLifecycle(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	a := &Service{store: db}
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_members,admin_sessions,admin_audit`); err != nil {
		t.Fatal(err)
	}
	call := func(action, email string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"action": action, "email": email})
		r := httptest.NewRequest("POST", "/api/admins", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(WithAdminActor(ctx, "google:owner:owner@example.test"))
		w := httptest.NewRecorder()
		a.AdminMembersHTTP(w, r, []string{"owner@example.test"})
		return w
	}
	for _, tc := range []struct {
		action, email string
		code          int
	}{
		{"add", "invalid", 400}, {"add", "Somebody <admin@example.test>", 400}, {"add", "OWNER@example.test", 403},
		{"disable", "owner@example.test", 403}, {"revoke", "owner@example.test", 403}, {"enable", "missing@example.test", 404},
		{"add", " ADMIN@example.test ", 200}, {"add", "admin@example.test", 409},
	} {
		w := call(tc.action, tc.email)
		if w.Code != tc.code {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	// A former owner's leftover session and token, which no longer pass the allow list, are cleared when the email is added as a member.
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_sessions(token_hash,subject,email,name,created_at) VALUES('former-owner','s','former@example.test','',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_tokens(hash,email,last4,expires_at) VALUES('former-owner-pat','former@example.test','abcd',now()+interval '30 days')`); err != nil {
		t.Fatal(err)
	}
	if w := call("add", "former@example.test"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var leftover int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_sessions WHERE email='former@example.test')+(SELECT count(*) FROM admin_tokens WHERE email='former@example.test')`).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatal("former owner's credentials survived the add", leftover, err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM admin_members WHERE email='former@example.test'`); err != nil {
		t.Fatal(err)
	}
	identity := AdminIdentity{Subject: "google-member", Email: "admin@example.test"}
	token, err := a.CreateAdminSession(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := a.AdminEmailAllowed(ctx, identity.Email); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if w := call("disable", identity.Email); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if ok, err := a.AdminEmailAllowed(ctx, identity.Email); err != nil || ok {
		t.Fatal(ok, err)
	}
	if _, err := a.AdminSession(ctx, token); err != ErrInvalid {
		t.Fatal("disabled session accepted", err)
	}
	if _, err := a.CreateAdminSession(ctx, identity); err != ErrInvalid {
		t.Fatal("disabled login accepted", err)
	}
	if w := call("enable", identity.Email); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := a.AdminSession(ctx, token); err != ErrInvalid {
		t.Fatal("old session revived", err)
	}
	token, err = a.CreateAdminSession(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if w := call("revoke", identity.Email); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := a.AdminSession(ctx, token); err != ErrInvalid {
		t.Fatal("revoked session accepted", err)
	}
	w := httptest.NewRecorder()
	a.AdminMembersHTTP(w, httptest.NewRequest("GET", "/api/admins", nil), []string{"owner@example.test"})
	var list struct {
		Owners []string
		Items  []struct {
			Email    string
			Enabled  bool
			Sessions int
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 1 || !list.Items[0].Enabled || list.Items[0].Sessions != 0 || len(list.Owners) != 1 {
		t.Fatal(w.Body.String())
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE actor='google:owner:owner@example.test' AND target='admin@example.test'`).Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email) SELECT 'member-'||i||'@example.test' FROM generate_series(1,99) i`); err != nil {
		t.Fatal(err)
	}
	if w := call("add", "overflow@example.test"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("add", "admin@example.test"); w.Code != 409 || !strings.Contains(w.Body.String(), "admin_limit") {
		t.Fatal("人数已满时应优先返回容量错误", w.Code, w.Body.String())
	}
}
