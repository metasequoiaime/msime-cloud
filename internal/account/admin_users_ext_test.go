package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// usersTestStore is a migrated store with the console tables the users tests read reset as well.
func usersTestStore(t *testing.T) (*Store, *Service) {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit,admin_members,user_preferences`); err != nil {
		t.Fatal(err)
	}
	return db, &Service{store: db}
}

// usersCall dispatches one console request with the given access.
func usersCall(a *Service, access AdminAccess, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithAdminAccess(r.Context(), access))
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r)
	return w
}

var usersOwner = AdminAccess{Actor: "google:sub-123:owner@example.test", Email: "owner@example.test", Role: RoleMaintainer, Permissions: AllAdminPermissions()}

func usersAuditCount(t *testing.T, db *Store, action string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE action=$1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func moderationOf(t *testing.T, db *Store, table, id string) (state, previous, reason string) {
	t.Helper()
	if err := db.pool.QueryRow(t.Context(), `SELECT moderation,COALESCE(previous_moderation,''),COALESCE(moderation_reason,'') FROM `+table+` WHERE id=$1`, id).Scan(&state, &previous, &reason); err != nil {
		t.Fatal(table, err)
	}
	return
}

func TestAdminBanAndUnbanUser(t *testing.T) {
	db, a := usersTestStore(t)
	ctx := t.Context()
	spammer := complete(t, db, Identity{"email", "spam@example.test"})
	second := complete(t, db, Identity{"email", "spam@example.test"})
	bystander := complete(t, db, Identity{"email", "fine@example.test"})
	uid := spammer.User.ID
	// One row per community table in each moderation state: approved, pending, already removed by a moderator, and another user's content.
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('ban-skin',$1,'Skin','{}'),('other-skin',$2,'Other','{}')`, uid, bystander.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('ban-dict',$1,'dictionary','Dict','{"entries":[]}','pending')`, uid); err != nil {
		t.Fatal(err)
	}
	insertCandidateSkin(t, db, "bd334455-1234-4234-8234-123456789abc", uid, "Candidate")
	if _, err := db.pool.Exec(ctx, `UPDATE community_candidate_skins SET moderation='removed',previous_moderation='approved',moderation_reason='spam',moderated_by='google:x:mod@example.test' WHERE id='bd334455-1234-4234-8234-123456789abc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('bd334455-1234-4234-8234-000000000001',$1,'sound','spam.sound','Plugin','1.0','MIT','id = "spam.sound"','zip',$2)`, uid, hash("plugin")); err != nil {
		t.Fatal(err)
	}

	// Failures are answered before anything changes and leave no audit row.
	for body, status := range map[string]int{
		`{"action":"ban_user","id":"` + uid + `"}`:                                             400,
		`{"action":"ban_user","id":"` + uid + `","reason":"  "}`:                               400,
		`{"action":"ban_user","id":"missing","reason":"spam"}`:                                 404,
		`{"action":"unban_user","id":"` + uid + `"}`:                                           409,
		`{"action":"unban_user","id":"missing"}`:                                               404,
		`{"action":"ban_user","reason":"spam"}`:                                                400,
		`{"action":"ban_user","id":"` + uid + `","reason":"` + strings.Repeat("x", 501) + `"}`: 400,
	} {
		if w := usersCall(a, usersOwner, "POST", "/api/actions", body); w.Code != status {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	reviewer := AdminAccess{Actor: "google:r:reviewer@example.test", Email: "reviewer@example.test", Role: "reviewer", Permissions: []string{PermReviewCommunity}}
	for _, action := range []string{"ban_user", "unban_user"} {
		if w := usersCall(a, reviewer, "POST", "/api/actions", `{"action":"`+action+`","id":"`+uid+`","reason":"spam"}`); w.Code != 403 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	if n := usersAuditCount(t, db, "ban_user") + usersAuditCount(t, db, "unban_user"); n != 0 {
		t.Fatal("failed ban audited", n)
	}

	w := usersCall(a, usersOwner, "POST", "/api/actions", `{"action":"ban_user","id":"`+uid+`","reason":"发布广告导流：多次"}`)
	var banned struct {
		OK       bool  `json:"ok"`
		Affected int64 `json:"affected"`
		Sessions int64 `json:"sessions"`
		Removed  int64 `json:"removed"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &banned) != nil || !banned.OK || banned.Affected != 1 || banned.Sessions != 2 || banned.Removed != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	var reason, by string
	if err := db.pool.QueryRow(ctx, `SELECT ban_reason,banned_by FROM auth_users WHERE id=$1 AND banned_at IS NOT NULL`, uid).Scan(&reason, &by); err != nil || reason != "发布广告导流：多次" || by != usersOwner.Actor {
		t.Fatal(reason, by, err)
	}
	for _, token := range []string{spammer.AccessToken, second.AccessToken} {
		if _, err := db.Authenticate(ctx, token); !errors.Is(err, ErrInvalid) {
			t.Fatal("session survived the ban", err)
		}
	}
	if _, err := db.Authenticate(ctx, bystander.AccessToken); err != nil {
		t.Fatal("other user affected", err)
	}
	for table, want := range map[string][3]string{
		"community_skins":           {"removed", "approved", "owner_banned"},
		"community_resources":       {"removed", "pending", "owner_banned"},
		"community_candidate_skins": {"removed", "approved", "spam"},
		"community_plugins":         {"removed", "approved", "owner_banned"},
	} {
		id := map[string]string{"community_skins": "ban-skin", "community_resources": "ban-dict", "community_candidate_skins": "bd334455-1234-4234-8234-123456789abc", "community_plugins": "bd334455-1234-4234-8234-000000000001"}[table]
		if state, previous, why := moderationOf(t, db, table, id); [3]string{state, previous, why} != want {
			t.Fatal(table, state, previous, why)
		}
	}
	if state, _, _ := moderationOf(t, db, "community_skins", "other-skin"); state != "approved" {
		t.Fatal("other user's content removed", state)
	}
	// The ban is not a review: the removed rows keep their reviewer and review time, so the personal page's stats do not count it.
	var reviewed int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT moderated_by FROM community_skins WHERE owner_id=$1 UNION ALL SELECT moderated_by FROM community_resources WHERE owner_id=$1 UNION ALL SELECT moderated_by FROM community_plugins WHERE owner_id=$1) x WHERE moderated_by IS NOT NULL`, uid).Scan(&reviewed); err != nil || reviewed != 0 {
		t.Fatal("ban stamped a reviewer", reviewed, err)
	}
	var detail map[string]any
	var raw []byte
	if err := db.pool.QueryRow(ctx, `SELECT detail FROM admin_audit WHERE action='ban_user' AND target=$1 AND actor=$2`, uid, usersOwner.Actor).Scan(&raw); err != nil || json.Unmarshal(raw, &detail) != nil || detail["reason"] != "发布广告导流：多次" || detail["removed"] != float64(3) || detail["name"] == "" {
		t.Fatal(string(raw), err)
	}

	// A second ban is a conflict; refresh and login of the banned account report the ban.
	if w := usersCall(a, usersOwner, "POST", "/api/actions", `{"action":"ban_user","id":"`+uid+`","reason":"again"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "already_banned") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.Refresh(ctx, spammer.RefreshToken); !errors.Is(err, ErrBanned) {
		t.Fatal("refresh of a banned account", err)
	}
	refresh := httptest.NewRecorder()
	a.refresh(refresh, jsonRequest("POST", "/v1/auth/refresh", `{"refresh_token":"`+second.RefreshToken+`"}`, ""))
	if refresh.Code != 403 || !strings.Contains(refresh.Body.String(), "account_banned") {
		t.Fatal(refresh.Code, refresh.Body.String())
	}
	challenge := Challenge{IDHash: hash(randomToken()), Provider: "email", Subject: "spam@example.test"}
	if err := db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Complete(ctx, challenge, Identity{"email", "spam@example.test"}); !errors.Is(err, ErrBanned) {
		t.Fatal("login of a banned account", err)
	}
	var challenges int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM auth_challenges WHERE id_hash=$1`, challenge.IDHash).Scan(&challenges); err != nil || challenges != 0 {
		t.Fatal("banned login left its challenge usable", challenges, err)
	}
	var sessions int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id=$1 AND NOT revoked`, uid).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("banned login created a session", sessions, err)
	}
	// Linking a new identity to a banned account is refused as well.
	link := Challenge{IDHash: hash(randomToken()), Provider: "email", Subject: "new@example.test", LinkUser: uid}
	if err := db.PutChallenge(ctx, link); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Complete(ctx, link, Identity{"email", "new@example.test"}); !errors.Is(err, ErrBanned) {
		t.Fatal("link to a banned account", err)
	}

	// Unbanning restores exactly what the ban removed and lets the account sign in again.
	w = usersCall(a, usersOwner, "POST", "/api/actions", `{"action":"unban_user","id":"`+uid+`"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restored":3`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for table, want := range map[string]string{"community_skins": "approved", "community_resources": "pending", "community_candidate_skins": "removed", "community_plugins": "approved"} {
		id := map[string]string{"community_skins": "ban-skin", "community_resources": "ban-dict", "community_candidate_skins": "bd334455-1234-4234-8234-123456789abc", "community_plugins": "bd334455-1234-4234-8234-000000000001"}[table]
		if state, previous, why := moderationOf(t, db, table, id); state != want || (table != "community_candidate_skins" && (previous != "" || why != "")) {
			t.Fatal(table, state, previous, why)
		}
	}
	var pendingReviewer *string
	if err := db.pool.QueryRow(ctx, `SELECT moderated_by FROM community_resources WHERE id='ban-dict'`).Scan(&pendingReviewer); err != nil || pendingReviewer != nil {
		t.Fatal("restored pending row has a reviewer", pendingReviewer, err)
	}
	if _, _, why := moderationOf(t, db, "community_candidate_skins", "bd334455-1234-4234-8234-123456789abc"); why != "spam" {
		t.Fatal("moderator removal undone by unban", why)
	}
	if w := usersCall(a, usersOwner, "POST", "/api/actions", `{"action":"unban_user","id":"`+uid+`"}`); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	again := Challenge{IDHash: hash(randomToken()), Provider: "email", Subject: "spam@example.test"}
	if err := db.PutChallenge(ctx, again); err != nil {
		t.Fatal(err)
	}
	if tokens, err := db.Complete(ctx, again, Identity{"email", "spam@example.test"}); err != nil || tokens.User.ID != uid {
		t.Fatal("unbanned account cannot sign in", err)
	}
	if n := usersAuditCount(t, db, "ban_user"); n != 1 {
		t.Fatal("ban audits", n)
	}
	if n := usersAuditCount(t, db, "unban_user"); n != 1 {
		t.Fatal("unban audits", n)
	}

	// The detail shows the ban history with a display actor, never the Google subject.
	detailBody := usersCall(a, usersOwner, "GET", "/api/users/"+uid, "").Body.String()
	if !strings.Contains(detailBody, `"action":"ban_user"`) || !strings.Contains(detailBody, `"actor":"owner@example.test"`) || strings.Contains(detailBody, "sub-123") || !strings.Contains(detailBody, `"section":"plugins"`) {
		t.Fatal(detailBody)
	}
}

// A session that is valid while its account is banned (a ban written outside the console) is rejected by authentication too.
func TestAuthenticateRejectsBannedAccount(t *testing.T) {
	db, _ := usersTestStore(t)
	user := complete(t, db, Identity{"email", "direct@example.test"})
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now(),ban_reason='manual' WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Authenticate(t.Context(), user.AccessToken); !errors.Is(err, ErrBanned) || !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := db.Refresh(t.Context(), user.RefreshToken); !errors.Is(err, ErrBanned) {
		t.Fatal(err)
	}
}

// An authenticated route answers a session whose account was banned outside the console with 403 account_banned, not 401.
func TestPrincipalAnswersBannedAccountWith403(t *testing.T) {
	db, a := usersTestStore(t)
	user := complete(t, db, Identity{"email", "direct-http@example.test"})
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now(),ban_reason='manual' WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/users/me", nil)
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	Route(a, "GET /v1/users/me", (*Service).me)(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"account_banned"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.error(w, ErrInvalid)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_credentials") {
		t.Fatal("plain invalid credentials must stay 401", w.Code, w.Body.String())
	}
}

// Login over HTTP answers a banned account with 403 account_banned and records the User-Agent on new sessions.
func TestLoginRecordsUserAgentAndRejectsBanned(t *testing.T) {
	db, _ := usersTestStore(t)
	t.Setenv("AUTH_TEST_PEPPER", strings.Repeat("p", 32))
	sender := &captureSender{}
	a := &Service{store: db, config: Config{PepperEnv: "AUTH_TEST_PEPPER", Email: MailConfig{From: "login@example.test"}}, sender: sender}
	mux := http.NewServeMux()
	Mount(mux, a)
	login := func(userAgent string) *httptest.ResponseRecorder {
		// Each login asks for a fresh code for the same address; the per-target resend limit is not under test here.
		if _, err := db.pool.Exec(t.Context(), `TRUNCATE auth_rates`); err != nil {
			t.Fatal(err)
		}
		w := apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"email","target":"device@qq.com"}`, "", 201)
		var c struct {
			ID string `json:"challenge_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		r := jsonRequest("POST", "/v1/auth/login", `{"challenge_id":"`+c.ID+`","credential":"`+sender.code+`"}`, "")
		r.Header.Set("User-Agent", userAgent)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	long := "MSIME/1.0 (Windows 11)\x01" + strings.Repeat("界", 300)
	w := login(long)
	var tokens Tokens
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	var stored string
	if err := db.pool.QueryRow(t.Context(), `SELECT user_agent FROM auth_sessions WHERE user_id=$1`, tokens.User.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "MSIME/1.0 (Windows 11)界") || len([]rune(stored)) != 256 {
		t.Fatal(len([]rune(stored)), stored)
	}
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now() WHERE id=$1`, tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	if w := login("MSIME/1.0"); w.Code != 403 || !strings.Contains(w.Body.String(), "account_banned") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSessionUserAgentSanitizing(t *testing.T) {
	for in, want := range map[string]string{"": "", " MSIME/1.0 ": "MSIME/1.0", "a\x00b\nc": "abc", "bad\xffutf8": "badutf8"} {
		if got := sessionUserAgent(withSessionUserAgent(context.Background(), in)); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
	if got := sessionUserAgent(context.Background()); got != "" {
		t.Fatal(got)
	}
}

func TestAdminUsersListStatsAndDetail(t *testing.T) {
	db, a := usersTestStore(t)
	ctx := t.Context()
	maintainer := complete(t, db, Identity{"email", "maint@example.test"})
	reviewer := complete(t, db, Identity{"google", "google-subject-1"})
	phone := complete(t, db, Identity{"phone", "+8613812342201"})
	plain := complete(t, db, Identity{"email", "plain@example.test"})
	disabled := complete(t, db, Identity{"email", "former@example.test"})
	unverified := complete(t, db, Identity{"google", "google-subject-2"})
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE auth_identities SET email='Review@Example.test',email_verified=true WHERE subject='google-subject-1'`, nil},
		{`UPDATE auth_identities SET email='maint@example.test',email_verified=false WHERE subject='google-subject-2'`, nil},
		{`INSERT INTO admin_members(email,role,enabled) VALUES('maint@example.test','maintainer',true),('review@example.test','reviewer',true),('former@example.test','operator',false)`, nil},
		{`INSERT INTO user_preferences(user_id) VALUES($1),($2)`, []any{maintainer.User.ID, plain.User.ID}},
		{`UPDATE auth_users SET created_at=now()-interval '30 days' WHERE id=$1`, []any{phone.User.ID}},
		{`UPDATE auth_users SET banned_at=now(),ban_reason='spam' WHERE id=$1`, []any{plain.User.ID}},
	} {
		if _, err := db.pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(statement.sql, err)
		}
	}
	readonly := AdminAccess{Actor: "pat:ro@example.test", Email: "ro@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}}

	stats := usersCall(a, readonly, "GET", "/api/users/stats", "")
	var s struct {
		Total     int64            `json:"total"`
		New7d     int64            `json:"new_7d"`
		SyncRatio float64          `json:"sync_ratio"`
		Banned    int64            `json:"banned"`
		Roles     map[string]int64 `json:"roles"`
	}
	if stats.Code != 200 || json.Unmarshal(stats.Body.Bytes(), &s) != nil || s.Total != 6 || s.New7d != 5 || s.Banned != 1 || s.SyncRatio < 0.33 || s.SyncRatio > 0.34 || s.Roles["maintainer"] != 1 || s.Roles["reviewer"] != 1 || s.Roles["user"] != 4 {
		t.Fatal(stats.Code, stats.Body.String())
	}
	if w := usersCall(a, readonly, "POST", "/api/users/stats", `{}`); w.Code != 405 {
		t.Fatal(w.Code)
	}

	type row struct {
		ID          string  `json:"id"`
		Role        string  `json:"role"`
		Contact     string  `json:"contact"`
		ContactKind string  `json:"contact_kind"`
		Devices     int     `json:"devices"`
		Sessions    int     `json:"sessions"`
		LastActive  *string `json:"last_active"`
		Banned      bool    `json:"banned"`
		BanReason   string  `json:"ban_reason"`
	}
	list := func(query string) []row {
		w := usersCall(a, readonly, "GET", "/api/users"+query, "")
		var page struct {
			Items []row `json:"items"`
			Total int   `json:"total"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != len(page.Items) {
			t.Fatal(query, w.Code, w.Body.String())
		}
		for _, secret := range []string{"maint@example.test", "plain@example.test", "13812342201", "google-subject"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("unmasked contact in list", secret, w.Body.String())
			}
		}
		return page.Items
	}
	byID := map[string]row{}
	for _, r := range list("") {
		byID[r.ID] = r
	}
	for id, want := range map[string]row{
		maintainer.User.ID: {Role: "maintainer", Contact: "m***@example.test", ContactKind: "email"},
		reviewer.User.ID:   {Role: "reviewer", Contact: "R***@Example.test", ContactKind: "email"},
		phone.User.ID:      {Role: "user", Contact: "+86****2201", ContactKind: "phone"},
		plain.User.ID:      {Role: "user", Contact: "p***@example.test", ContactKind: "email", Banned: true, BanReason: "spam"},
		disabled.User.ID:   {Role: "user", Contact: "f***@example.test", ContactKind: "email"},
		unverified.User.ID: {Role: "user", Contact: "m***@example.test", ContactKind: "email"},
	} {
		got := byID[id]
		if got.Role != want.Role || got.Contact != want.Contact || got.ContactKind != want.ContactKind || got.Banned != want.Banned || got.BanReason != want.BanReason || got.Devices != 1 || got.Sessions != 1 || got.LastActive == nil {
			t.Fatalf("%s: %+v want %+v", id, got, want)
		}
	}
	if items := list("?role=maintainer"); len(items) != 1 || items[0].ID != maintainer.User.ID {
		t.Fatal(items)
	}
	if items := list("?role=user"); len(items) != 4 {
		t.Fatal(items)
	}
	if items := list("?q=****2201"); len(items) != 1 || items[0].ID != phone.User.ID {
		t.Fatal(items)
	}
	for _, query := range []string{"?role=" + strings.Repeat("r", 33), "?visibility=public"} {
		if w := usersCall(a, readonly, "GET", "/api/users"+query, ""); w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
	if w := usersCall(a, readonly, "GET", "/api/skins?role=user", ""); w.Code != 400 {
		t.Fatal("role filter accepted by another list", w.Code)
	}

	// The detail carries the masked contact, role, sync state, session user agents and works, without identity subjects.
	for _, statement := range []string{
		`UPDATE auth_sessions SET user_agent='MSIME/0.5.4 (Windows 11)' WHERE user_id=$1`,
		`INSERT INTO community_skins(id,owner_id,name,design) VALUES('detail-skin',$1,'水杉秋色','{}')`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('detail-reply',$1,'reply','客服安抚模板','{"prompt":"hi"}')`,
	} {
		if _, err := db.pool.Exec(ctx, statement, maintainer.User.ID); err != nil {
			t.Fatal(statement, err)
		}
	}
	w := usersCall(a, readonly, "GET", "/api/users/"+maintainer.User.ID, "")
	var detail struct {
		Role     string `json:"role"`
		Contact  string `json:"contact"`
		Sync     bool   `json:"sync"`
		Banned   bool   `json:"banned"`
		Sessions []struct {
			UserAgent string `json:"user_agent"`
			Status    string `json:"status"`
		} `json:"sessions"`
		Works []struct {
			Section    string `json:"section"`
			Name       string `json:"name"`
			Moderation string `json:"moderation"`
		} `json:"works"`
		History []any `json:"history"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Role != "maintainer" || detail.Contact != "m***@example.test" || !detail.Sync || detail.Banned ||
		len(detail.Sessions) != 1 || detail.Sessions[0].UserAgent != "MSIME/0.5.4 (Windows 11)" || len(detail.Works) != 2 || len(detail.History) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	sections := map[string]bool{}
	for _, work := range detail.Works {
		sections[work.Section] = work.Moderation == "approved"
	}
	if !sections["skins"] || !sections["replies"] {
		t.Fatal(detail.Works)
	}
	if strings.Contains(w.Body.String(), "maint@example.test") {
		t.Fatal("unmasked contact in detail")
	}
	if w := usersCall(a, readonly, "GET", "/api/users/"+reviewer.User.ID, ""); w.Code != 200 || strings.Contains(w.Body.String(), "google-subject") {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A login racing a ban waits for the ban to commit and is then refused, instead of reading the pre-ban row and creating a session the ban's revocation never saw.
func TestLoginWaitsForConcurrentBan(t *testing.T) {
	db, _ := usersTestStore(t)
	ctx := t.Context()
	user := complete(t, db, Identity{"email", "racer@example.test"})
	ban, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ban.Rollback(ctx)
	if _, err = ban.Exec(ctx, `UPDATE auth_users SET banned_at=now(),ban_reason='race' WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ban.Exec(ctx, `UPDATE auth_sessions SET revoked=true WHERE user_id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	challenge := Challenge{IDHash: hash(randomToken()), Provider: "email", Subject: "racer@example.test"}
	if err = db.PutChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := db.Complete(context.Background(), challenge, Identity{"email", "racer@example.test"})
		result <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting bool
		if err = db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT banned_at IS NOT NULL%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatal("login finished before the ban committed", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("login never waited on the ban")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = ban.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrBanned) {
		t.Fatal("login racing a ban", err)
	}
	var sessions int
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id=$1 AND NOT revoked`, user.User.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("session survived the ban", sessions, err)
	}
}

// A short phone number keeps only two trailing digits, so the masked contact never reveals most of the number.
func TestAdminUsersShortPhoneMasking(t *testing.T) {
	db, a := usersTestStore(t)
	short := complete(t, db, Identity{"phone", "+123456789"})
	w := usersCall(a, usersOwner, "GET", "/api/users/"+short.User.ID, "")
	var detail struct {
		Contact string `json:"contact"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Contact != "+12****89" {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A configured owner has no admin_members row, yet the list, the role chips and the detail show the owner's account as a maintainer; an unverified email claiming the owner address does not.
func TestAdminUsersCountConfiguredOwnersAsMaintainers(t *testing.T) {
	db, a := usersTestStore(t)
	a.ConfigureAdmin(AdminSettings{Owners: []string{"boss@example.test"}})
	owner := complete(t, db, Identity{"email", "boss@example.test"})
	claimant := complete(t, db, Identity{"google", "google-claimant"})
	plain := complete(t, db, Identity{"email", "someone@example.test"})
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_identities SET email='boss@example.test',email_verified=false WHERE subject='google-claimant'`); err != nil {
		t.Fatal(err)
	}
	readonly := AdminAccess{Actor: "pat:ro@example.test", Email: "ro@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}}

	var stats struct {
		Roles map[string]int64 `json:"roles"`
	}
	if w := usersCall(a, readonly, "GET", "/api/users/stats", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats.Roles["maintainer"] != 1 || stats.Roles["user"] != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	var page struct {
		Items []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"items"`
	}
	if w := usersCall(a, readonly, "GET", "/api/users?role=maintainer", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != owner.User.ID {
		t.Fatal(w.Code, w.Body.String())
	}
	for id, want := range map[string]string{owner.User.ID: "maintainer", claimant.User.ID: "user", plain.User.ID: "user"} {
		var detail struct {
			Role string `json:"role"`
		}
		if w := usersCall(a, readonly, "GET", "/api/users/"+id, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Role != want {
			t.Fatal(id, want, w.Code, w.Body.String())
		}
	}
	// Without configured owners the same account is an ordinary user again, so the role comes from the configuration rather than the data.
	a.ConfigureAdmin(AdminSettings{})
	if w := usersCall(a, readonly, "GET", "/api/users?role=maintainer", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
}
