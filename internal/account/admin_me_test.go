package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSafariUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15"

func truncateAdminPersonal(t *testing.T, db *Store) {
	t.Helper()
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE admin_members,admin_sessions,admin_audit,admin_tokens,admin_preferences`); err != nil {
		t.Fatal(err)
	}
}

// meRequest 用已认证的会话或个人令牌身份访问个人中心；Cookie 会话同时绑定来源凭据。
func meRequest(a *Service, access AdminAccess, method, body, cookie string) *httptest.ResponseRecorder {
	r := jsonRequest(method, "/api/me", body, "")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-msime_admin", Value: cookie})
		r = r.WithContext(WithAdminSession(r.Context(), cookie))
	}
	r = r.WithContext(WithAdminAccess(r.Context(), access))
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r)
	return w
}

type meResponse struct {
	Email    string     `json:"email"`
	Name     string     `json:"name"`
	Role     string     `json:"role"`
	Via      string     `json:"via"`
	JoinedAt *time.Time `json:"joined_at"`
	Stats    struct {
		DictPRs        int64    `json:"dict_prs_month"`
		Community      int64    `json:"community_month"`
		Issues         int64    `json:"issues_month"`
		AvgHandleHours *float64 `json:"avg_handle_hours"`
	} `json:"stats"`
	Prefs    map[string]bool   `json:"prefs"`
	Recent   []adminAuditEntry `json:"recent"`
	Sessions []struct {
		ID      string `json:"id"`
		Device  string `json:"device"`
		Current bool   `json:"current"`
	} `json:"sessions"`
	Token *adminTokenInfo `json:"token"`
}

func decodeMe(t *testing.T, w *httptest.ResponseRecorder) meResponse {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var v meResponse
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAdminMePage(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	a := &Service{store: db}
	truncateAdminPersonal(t, db)
	email := "member@example.test"
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email,role) VALUES($1,'reviewer')`, email); err != nil {
		t.Fatal(err)
	}
	current, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g1", Email: email, Name: "Member\x00 Name", UserAgent: testSafariUA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.CreateAdminSession(ctx, AdminIdentity{Subject: "g1", Email: email, UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0"}); err != nil {
		t.Fatal(err)
	}
	// Another admin's session and audit rows never show up.
	if _, err = a.CreateAdminSession(ctx, AdminIdentity{Subject: "g2", Email: "other@example.test"}); err != nil {
		t.Fatal(err)
	}
	actor := "google:g1:" + email
	user := complete(t, db, Identity{"email", "author@example.test"})
	for _, q := range []string{
		`INSERT INTO admin_audit(action,target,actor,detail) VALUES('dict_pr_approve','210','` + actor + `','{"count":5}'),('dict_pr_trim','210','pat:` + email + `','{}'),('dict_pr_reject','211','` + actor + `','{}'),('issue_triage','msime#1','` + actor + `','{}'),('issue_comment','msime#1','` + actor + `','{}'),('delete_reply','r1','` + actor + `','{}')`,
		`INSERT INTO admin_audit(action,target,actor,created_at) VALUES('dict_pr_approve','1','` + actor + `',now()-interval '40 days'),('dict_pr_approve','2','google:g2:other@example.test',now()),('dict_pr_approve','3','google:g3:xmember@example.test',now())`,
		`INSERT INTO community_skins(id,owner_id,name,design,created_at,moderation,moderated_by,moderated_at) VALUES('me-skin',$1,'Skin','{}',now()-interval '4 hours','approved','` + actor + `',now()),('me-skin-2',$1,'Skin','{}',now()-interval '2 hours','removed','` + actor + `',now()),('me-skin-3',$1,'Skin','{}',now(),'approved','google:g2:other@example.test',now())`,
	} {
		args := []any{}
		if strings.Contains(q, "$1") {
			args = append(args, user.User.ID)
		}
		if _, err = db.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	access := AdminAccess{Actor: actor, Email: email, Role: "reviewer", Permissions: []string{PermReviewCommunity}}
	me := decodeMe(t, meRequest(a, access, "GET", "", current))
	if me.Email != email || me.Name != "Member Name" || me.Role != "reviewer" || me.Via != "session" || me.JoinedAt == nil {
		t.Fatalf("%+v", me)
	}
	if me.Stats.DictPRs != 2 || me.Stats.Issues != 1 || me.Stats.Community != 3 || me.Stats.AvgHandleHours == nil || *me.Stats.AvgHandleHours < 2.9 || *me.Stats.AvgHandleHours > 3.1 {
		t.Fatalf("stats %+v avg %v", me.Stats, me.Stats.AvgHandleHours)
	}
	if len(me.Recent) != 6 || me.Recent[0].CreatedAt.Before(me.Recent[5].CreatedAt) {
		t.Fatalf("recent %+v", me.Recent)
	}
	if len(me.Sessions) != 2 || me.Token != nil {
		t.Fatalf("%+v", me)
	}
	devices := map[string]bool{}
	currentCount := 0
	for _, s := range me.Sessions {
		devices[s.Device] = true
		if s.Current {
			currentCount++
		}
	}
	if !devices["macOS · Safari"] || !devices["Windows · Edge"] || currentCount != 1 {
		t.Fatalf("sessions %+v", me.Sessions)
	}
	if !me.Prefs["notify_dict_pr"] || !me.Prefs["notify_report"] || !me.Prefs["notify_crash_spike"] || me.Prefs["weekly_digest"] || len(me.Prefs) != 4 {
		t.Fatalf("prefs %+v", me.Prefs)
	}

	// Preferences merge into the stored object and are audited.
	for body, code := range map[string]int{
		`{"action":"set_pref","key":"unknown","value":true}`:                 400,
		`{"action":"set_pref","key":"weekly_digest"}`:                        400,
		`{"action":"bogus"}`:                                                 400,
		`{"action":"set_pref","key":"weekly_digest","value":true,"extra":1}`: 400,
	} {
		if w := meRequest(a, access, "POST", body, ""); w.Code != code {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO admin_preferences(email,prefs) VALUES($1,'{"theme":"dark"}')`, email); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"action":"set_pref","key":"weekly_digest","value":true}`, `{"action":"set_pref","key":"notify_report","value":false}`} {
		if w := meRequest(a, access, "POST", body, ""); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var theme string
	if err = db.pool.QueryRow(ctx, `SELECT prefs->>'theme' FROM admin_preferences WHERE email=$1`, email).Scan(&theme); err != nil || theme != "dark" {
		t.Fatal("unrelated preference lost", theme, err)
	}
	me = decodeMe(t, meRequest(a, access, "GET", "", ""))
	if !me.Prefs["weekly_digest"] || me.Prefs["notify_report"] || len(me.Prefs) != 4 {
		t.Fatalf("prefs %+v", me.Prefs)
	}
	if me.Recent[0].Action != "admin_pref_set" {
		t.Fatalf("pref change not audited: %+v", me.Recent[0])
	}

	// Sessions are revoked by their public id, only the caller's own.
	var otherID, ownID string
	if err = db.pool.QueryRow(ctx, `SELECT id FROM admin_sessions WHERE email='other@example.test'`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	if err = db.pool.QueryRow(ctx, `SELECT id FROM admin_sessions WHERE email=$1 AND token_hash<>$2`, email, hash(current)).Scan(&ownID); err != nil {
		t.Fatal(err)
	}
	for body, code := range map[string]int{
		`{"action":"revoke_session","id":"` + otherID + `"}`: 404,
		`{"action":"revoke_session","id":"zz"}`:              400,
		`{"action":"revoke_session","id":"` + ownID + `"}`:   200,
	} {
		if w := meRequest(a, access, "POST", body, ""); w.Code != code {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	if w := meRequest(a, access, "POST", `{"action":"revoke_session","id":"`+ownID+`"}`, ""); w.Code != 404 {
		t.Fatal("revoked twice", w.Code)
	}
	var audits int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE action='admin_session_revoke'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("failed revocations audited", audits, err)
	}
	if _, err = a.AdminSession(ctx, current); err != nil {
		t.Fatal("current session lost", err)
	}

	// The legacy token has no personal account.
	legacy := AdminAccess{Actor: "legacy-token", Role: RoleMaintainer, Permissions: AllAdminPermissions()}
	if me = decodeMe(t, meRequest(a, legacy, "GET", "", "")); me.Via != "legacy" || me.Email != "" || len(me.Sessions) != 0 || me.Token != nil {
		t.Fatalf("%+v", me)
	}
	if w := meRequest(a, legacy, "POST", `{"action":"regenerate_token"}`, ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestAdminPersonalAccessTokens(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	a := &Service{store: db}
	truncateAdminPersonal(t, db)
	email := "owner@example.test"
	session := AdminAccess{Actor: "google:g:" + email, Email: email, Role: RoleMaintainer, Permissions: AllAdminPermissions(), Owner: true}
	ownerCookie, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
	if err != nil {
		t.Fatal(err)
	}
	cookies := map[string]string{email: ownerCookie}
	regenerate := func(access AdminAccess) (int, map[string]any) {
		t.Helper()
		w := meRequest(a, access, "POST", `{"action":"regenerate_token"}`, cookies[access.Email])
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	code, first := regenerate(session)
	token, _ := first["token"].(string)
	if code != 200 || !strings.HasPrefix(token, AdminTokenPrefix) || len(token) != adminTokenLength || first["last4"] != token[len(token)-4:] {
		t.Fatal(code, first)
	}
	identity, err := a.AdminTokenIdentity(ctx, token)
	if err != nil || identity.Email != email {
		t.Fatal(identity, err)
	}
	// Only the hash is stored.
	var stored int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_tokens WHERE hash=$1 AND hash<>$2`, hash(token), token).Scan(&stored); err != nil || stored != 1 {
		t.Fatal(stored, err)
	}
	me := decodeMe(t, meRequest(a, session, "GET", "", ""))
	if me.Token == nil || me.Token.Last4 != token[len(token)-4:] || time.Until(me.Token.ExpiresAt) < 29*24*time.Hour {
		t.Fatalf("%+v", me.Token)
	}
	// A token cannot renew itself, so a leaked token expires.
	if code, _ = regenerate(AdminAccess{Actor: "pat:" + email, Email: email, Role: RoleMaintainer, Permissions: AllAdminPermissions()}); code != 403 {
		t.Fatal("token renewed itself", code)
	}
	// Regenerating revokes the previous token at once.
	_, second := regenerate(session)
	if _, err = a.AdminTokenIdentity(ctx, token); err != ErrInvalid {
		t.Fatal("old token still valid", err)
	}
	next := second["token"].(string)
	if _, err = a.AdminTokenIdentity(ctx, next); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", next[:len(next)-1], "x" + next[1:], strings.Repeat("a", adminTokenLength), AdminTokenPrefix + strings.Repeat("0", 64)} {
		if _, err = a.AdminTokenIdentity(ctx, bad); err != ErrInvalid {
			t.Fatal(bad, err)
		}
	}
	if _, err = db.pool.Exec(ctx, `UPDATE admin_tokens SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.AdminTokenIdentity(ctx, next); err != ErrInvalid {
		t.Fatal("expired token accepted", err)
	}
	if me = decodeMe(t, meRequest(a, session, "GET", "", "")); me.Token != nil {
		t.Fatal("expired token shown", me.Token)
	}
	var audits int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE action='admin_token_regenerate' AND detail ? 'last4'`).Scan(&audits); err != nil || audits != 2 {
		t.Fatal(audits, err)
	}

	// Disabling a member drops their tokens with their sessions.
	if _, err = db.pool.Exec(ctx, `INSERT INTO admin_members(email) VALUES('member@example.test')`); err != nil {
		t.Fatal(err)
	}
	member := AdminAccess{Actor: "google:m:member@example.test", Email: "member@example.test", Role: RoleMaintainer, Permissions: AllAdminPermissions()}
	memberCookie, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "m", Email: member.Email})
	if err != nil {
		t.Fatal(err)
	}
	cookies[member.Email] = memberCookie
	_, issued := regenerate(member)
	memberToken := issued["token"].(string)
	body := `{"action":"disable","email":"member@example.test"}`
	r := jsonRequest("POST", "/api/admins", body, "")
	r = r.WithContext(WithAdminActor(r.Context(), session.Actor))
	w := httptest.NewRecorder()
	a.AdminMembersHTTP(w, r, []string{email})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = a.AdminTokenIdentity(ctx, memberToken); err != ErrInvalid {
		t.Fatal("disabled member token survived", err)
	}
}

// Sessions record the browser and Google name, and last_seen_at moves at most every five minutes.
func TestAdminSessionMetadata(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	a := &Service{store: db}
	truncateAdminPersonal(t, db)
	token, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "s", Email: "Owner@Example.test", Name: strings.Repeat("名", 300), UserAgent: strings.Repeat("u", 400) + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := a.AdminSession(ctx, token)
	if err != nil || identity.Email != "owner@example.test" || len([]rune(identity.Name)) != 200 || len(identity.UserAgent) != 256 {
		t.Fatal(identity, err)
	}
	seen := func() time.Time {
		var at time.Time
		if err := db.pool.QueryRow(ctx, `SELECT last_seen_at FROM admin_sessions`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if _, err = db.pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at=now()-interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	before := seen()
	if _, err = a.AdminSession(ctx, token); err != nil || !seen().Equal(before) {
		t.Fatal("fresh session touched", err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at=now()-interval '6 minutes'`); err != nil {
		t.Fatal(err)
	}
	stale := seen()
	if _, err = a.AdminSession(ctx, token); err != nil || time.Since(seen()) > time.Minute || !seen().After(stale) {
		t.Fatal("stale session not touched", err)
	}
	for agent, want := range map[string]string{
		"":           "未知设备",
		testSafariUA: "macOS · Safari",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": "iPhone · Safari",
	} {
		if got := adminDevice(agent); got != want {
			t.Errorf("%q: %q want %q", agent, got, want)
		}
	}
}

// Concurrent regenerations must still leave exactly one valid token, so no older token stays usable without showing on the personal page.
func TestAdminTokenRegenerateConcurrent(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	a := &Service{store: db}
	truncateAdminPersonal(t, db)
	email := "owner@example.test"
	session := AdminAccess{Actor: "google:g:" + email, Email: email, Role: RoleMaintainer, Permissions: AllAdminPermissions(), Owner: true}
	cookie, err := a.CreateAdminSession(ctx, AdminIdentity{Subject: "g", Email: email})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	codes := make(chan int, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			codes <- meRequest(a, session, "POST", `{"action":"regenerate_token"}`, cookie).Code
		}()
	}
	close(start)
	for range workers {
		if code := <-codes; code != 200 {
			t.Fatal(code)
		}
	}
	var tokens int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_tokens WHERE email=$1`, email).Scan(&tokens); err != nil || tokens != 1 {
		t.Fatal("tokens left", tokens, err)
	}
}
