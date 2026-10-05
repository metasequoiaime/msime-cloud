package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// adminTestContext is the context the server gives an owner: every permission.
func adminTestContext(ctx context.Context, actor string) context.Context {
	return WithAdminAccess(ctx, AdminAccess{Actor: actor, Role: RoleMaintainer, Permissions: AllAdminPermissions()})
}

// adminJSONRequest is a JSON request carrying adminTestContext.
func adminJSONRequest(method, path, body string) *http.Request {
	r := jsonRequest(method, path, body, "")
	return r.WithContext(adminTestContext(r.Context(), "legacy-token"))
}

// The registries are the contract every console unit builds on: each action and route named by the plan is wired, to a handler, behind a known permission.
func TestAdminRegistryCompleteness(t *testing.T) {
	actions := []string{
		"revoke_session", "revoke_sessions", "ban_user", "unban_user",
		"delete_skin", "delete_candidate_skin", "delete_plugin", "delete_dictionary", "delete_reply", "delete_phrase", "approve_content", "remove_content", "restore_content", "set_skin_category", "set_candidate_skin_category",
		"add_sensitive_word", "set_sensitive_word_level", "delete_sensitive_word",
		"save_notice_draft", "publish_notice", "archive_notice",
		"resolve_crash", "reopen_crash", "crash_group_status",
		"open_incident", "resolve_incident", "update_incident",
		"resolve_feedback", "reopen_feedback",
	}
	var registered []string
	for name, spec := range adminActions {
		registered = append(registered, name)
		if spec.run == nil || (spec.perm != "" && !slices.Contains(AllAdminPermissions(), spec.perm)) {
			t.Errorf("action %s: missing handler or unknown permission %q", name, spec.perm)
		}
	}
	sort.Strings(actions)
	sort.Strings(registered)
	if !slices.Equal(actions, registered) {
		t.Fatalf("registered actions %v, want %v", registered, actions)
	}
	routes := map[string]bool{}
	for _, route := range adminRoutes {
		if route.handle == nil || (route.method != "GET" && route.method != "POST") || strings.Count(route.pattern, "{}") > 1 || strings.HasPrefix(route.pattern, "/") {
			t.Errorf("malformed route %+v", route)
		}
		routes[route.method+" "+route.pattern] = true
	}
	for _, want := range []string{"GET overview", "GET notifications", "POST notifications/read", "GET me", "POST me", "GET permissions", "POST permissions", "GET users/stats", "GET users/{}", "GET community/counts", "GET candidate-skins/{}/preview", "GET skins/{}", "GET candidate-skins/{}", "GET plugins/{}", "GET dictionaries/{}", "GET replies/{}", "GET phrases/{}", "GET sensitive-words", "GET downloads/summary", "GET notices", "GET crash-groups", "GET crash-groups/{}", "GET feedback/{}"} {
		if !routes[want] {
			t.Errorf("route %s not registered", want)
		}
	}
	// A specific pattern must precede the wildcard that also matches it.
	for i, route := range adminRoutes {
		for _, earlier := range adminRoutes[:i] {
			if _, ok := matchAdminPattern(earlier.pattern, strings.ReplaceAll(route.pattern, "{}", "x")); ok && earlier.method == route.method && earlier.pattern != route.pattern {
				t.Errorf("route %s is shadowed by %s", route.pattern, earlier.pattern)
			}
		}
	}
	field := regexp.MustCompile(`^[a-z_]+$`)
	for _, name := range []string{"users", "skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases", "downloads", "crashes", "audit", "feedback"} {
		list, ok := adminLists[name]
		if !ok || list.query == "" {
			t.Errorf("list %s not registered", name)
		}
		for _, f := range list.filters {
			if !field.MatchString(f.field) || !field.MatchString(f.param) || f.max <= 0 {
				t.Errorf("list %s: unsafe filter %+v", name, f)
			}
		}
	}
	for pattern, cases := range map[string]map[string]string{
		"users/{}":                   {"users/abc": "abc", "users/": "", "users/a/b": "a/b"},
		"candidate-skins/{}/preview": {"candidate-skins/x/preview": "x"},
		"overview":                   {"overview": ""},
	} {
		for path, want := range cases {
			if got, ok := matchAdminPattern(pattern, path); !ok || got != want {
				t.Errorf("%s on %s: %q %v", pattern, path, got, ok)
			}
		}
	}
	for pattern, path := range map[string]string{"candidate-skins/{}/preview": "candidate-skins/x", "overview": "overview/x", "users/{}": "user"} {
		if _, ok := matchAdminPattern(pattern, path); ok {
			t.Errorf("%s matched %s", pattern, path)
		}
	}
}

// Writes are gated by the caller's permissions and refused before anything runs or is audited; reads stay open to every role.
func TestAdminActionPermissions(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	user := complete(t, db, Identity{"email", "rbac-user@example.test"})
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('rbac-skin',$1,'Skin','{}')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	call := func(access *AdminAccess, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if access != nil {
			r = r.WithContext(WithAdminAccess(r.Context(), *access))
		}
		w := httptest.NewRecorder()
		a.AdminHTTP(w, r)
		return w
	}
	reviewer := &AdminAccess{Actor: "google:r:reviewer@example.test", Email: "reviewer@example.test", Role: "reviewer", Permissions: []string{PermReviewDictPR, PermReviewCommunity, PermTriageIssues}}
	readonly := &AdminAccess{Actor: "google:o:readonly@example.test", Email: "readonly@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}}
	for _, tc := range []struct {
		access *AdminAccess
		body   string
	}{
		{reviewer, `{"action":"revoke_sessions","id":"` + user.User.ID + `"}`},
		{readonly, `{"action":"delete_skin","id":"rbac-skin"}`},
		{nil, `{"action":"delete_skin","id":"rbac-skin"}`},
	} {
		if w := call(tc.access, "POST", "/api/actions", tc.body); w.Code != 403 || !strings.Contains(w.Body.String(), "permission_denied") {
			t.Fatal(tc.body, w.Code, w.Body.String())
		}
	}
	if _, err := db.Authenticate(ctx, user.AccessToken); err != nil {
		t.Fatal("a refused action changed data", err)
	}
	for _, path := range []string{"/api/users", "/api/skins", "/api/overview?days=7", "/api/audit"} {
		if w := call(readonly, "GET", path, ""); w.Code != 200 {
			t.Fatal("read-only role cannot read", path, w.Code)
		}
	}
	if w := call(reviewer, "POST", "/api/actions", `{"action":"delete_skin","id":"rbac-skin","reason":"spam"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var actor, detail string
	var audits int
	if err := db.pool.QueryRow(ctx, `SELECT count(*),min(actor),min(detail::text) FROM admin_audit`).Scan(&audits, &actor, &detail); err != nil || audits != 1 || actor != reviewer.Actor || detail != `{"reason": "spam"}` {
		t.Fatal(audits, actor, detail, err)
	}
}

// The action body grows ids, reason, section and value without loosening validation; a stub answers 501 and is never audited.
func TestAdminActionRequestContract(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	var got actionRequest
	adminActions["test_echo"] = adminActionSpec{PermReviewCommunity, func(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
		got = v
		return actionResult{Affected: int64(len(v.IDs)), Target: "batch", Detail: map[string]any{"count": len(v.IDs)}, Extra: map[string]any{"id": 7}}, nil
	}}
	adminActions["test_stub"] = adminActionSpec{"", func(*Service, context.Context, pgx.Tx, actionRequest) (actionResult, error) {
		return actionResult{}, errNotImplemented
	}}
	adminActions["test_denied"] = adminActionSpec{"", func(*Service, context.Context, pgx.Tx, actionRequest) (actionResult, error) {
		return actionResult{}, checkPerm(ctx, PermManagePermissions)
	}}
	t.Cleanup(func() {
		delete(adminActions, "test_echo")
		delete(adminActions, "test_stub")
		delete(adminActions, "test_denied")
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.AdminHTTP(w, r.WithContext(adminTestContext(r.Context(), "test-admin")))
	})
	w := apiRequest(t, handler, "POST", "/api/actions", `{"action":"test_echo","ids":["a","b"],"reason":"重复内容","section":"skins","value":{"level":"block"}}`, "", 200)
	var response map[string]any
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || response["ok"] != true || response["affected"] != 2.0 || response["id"] != 7.0 {
		t.Fatal(w.Body.String())
	}
	if !slices.Equal(got.IDs, []string{"a", "b"}) || got.Reason != "重复内容" || got.Section != "skins" || string(got.Value) != `{"level":"block"}` {
		t.Fatalf("%+v", got)
	}
	var target, detail string
	if err := db.pool.QueryRow(ctx, `SELECT target,detail::text FROM admin_audit WHERE action='test_echo'`).Scan(&target, &detail); err != nil || target != "batch" || detail != `{"count": 2}` {
		t.Fatal(target, detail, err)
	}
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "x"
	}
	tooMany, _ := json.Marshal(map[string]any{"action": "test_echo", "ids": ids})
	for _, body := range []string{string(tooMany), `{"action":"test_echo","ids":[""]}`, `{"action":"test_echo","reason":"` + strings.Repeat("长", 501) + `"}`, `{"action":"test_echo","section":"` + strings.Repeat("s", 65) + `"}`, `{"action":"test_echo","unknown":1}`, `{"action":"test_echo","id":"bad\nid"}`} {
		apiRequest(t, handler, "POST", "/api/actions", body, "", 400)
	}
	apiRequest(t, handler, "POST", "/api/actions", `{"action":"test_stub"}`, "", 501)
	apiRequest(t, handler, "POST", "/api/actions", `{"action":"test_denied"}`, "", 403)
	var audits int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("rejected actions were audited", audits, err)
	}
}

// Members resolve to their role's permissions; the built-in matrix is seeded once and a console edit survives re-running the migration, except that maintainers always keep permission management.
func TestAdminMemberRoleResolution(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_members`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_members(email) VALUES('old@example.test'); INSERT INTO admin_members(email,role) VALUES('reviewer@example.test','reviewer'); INSERT INTO admin_members(email,enabled,role) VALUES('off@example.test',false,'readonly')`); err != nil {
		t.Fatal(err)
	}
	role, perms, err := a.AdminMemberRole(ctx, "OLD@example.test")
	if err != nil || role != RoleMaintainer || !slices.Equal(perms, AllAdminPermissions()) {
		t.Fatal("a member from before roles must keep every permission", role, perms, err)
	}
	role, perms, err = a.AdminMemberRole(ctx, "reviewer@example.test")
	if err != nil || role != "reviewer" || !slices.Equal(perms, []string{PermReviewDictPR, PermReviewCommunity, PermTriageIssues}) {
		t.Fatal(role, perms, err)
	}
	for _, email := range []string{"off@example.test", "missing@example.test"} {
		if _, _, err = a.AdminMemberRole(ctx, email); !errors.Is(err, ErrInvalid) {
			t.Fatal(email, err)
		}
	}
	if _, err = db.pool.Exec(ctx, `DELETE FROM admin_role_permissions WHERE (role='reviewer' AND permission='triage_issues') OR (role='maintainer' AND permission='manage_permissions')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `INSERT INTO admin_role_permissions(role,permission) VALUES('reviewer','triage_issues') ON CONFLICT DO NOTHING`)
	})
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, perms, _ = a.AdminMemberRole(ctx, "reviewer@example.test"); slices.Contains(perms, PermTriageIssues) {
		t.Fatal("re-running the migration reset an edited role")
	}
	if _, perms, _ = a.AdminMemberRole(ctx, "old@example.test"); !slices.Contains(perms, PermManagePermissions) {
		t.Fatal("maintainers lost permission management")
	}
	if _, err = db.pool.Exec(ctx, `UPDATE admin_members SET role='nonexistent' WHERE email='old@example.test'`); err == nil {
		t.Fatal("a member was given an undefined role")
	}
}

func TestAdminAuditDetail(t *testing.T) {
	db := testStore(t)
	ctx := WithAdminActor(context.Background(), "pat:someone@example.test")
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	if err := a.Audit(ctx, "dict_pr_merge", "210", map[string]any{"entries": 5}); err != nil {
		t.Fatal(err)
	}
	if err := a.Audit(ctx, "bad", "x", []int{1}); err == nil {
		t.Fatal("non-object detail accepted")
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = auditTx(ctx, tx, "rolled_back", "x", nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	var detail, actor string
	if err = db.pool.QueryRow(ctx, `SELECT count(*),min(detail::text),min(actor) FROM admin_audit`).Scan(&count, &detail, &actor); err != nil || count != 1 || detail != `{"entries": 5}` || actor != "pat:someone@example.test" {
		t.Fatal(count, detail, actor, err)
	}
}

// Content that existed before moderation stays approved, and the telemetry table accepts the new optional columns and kinds.
func TestAdminConsoleSchemaDefaults(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := complete(t, db, Identity{"email", "schema@example.test"})
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO community_skins(id,owner_id,name,design) VALUES('schema-skin',$1,'Skin','{}')`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	var moderation string
	if err := db.pool.QueryRow(ctx, `SELECT moderation FROM community_skins WHERE id='schema-skin'`).Scan(&moderation); err != nil || moderation != "approved" {
		t.Fatal(moderation, err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version,install_id,artifact,channel,signature) VALUES('schema-active-0001','active','windows','1','install-0123456789','setup.exe','mirror','0123456789abcdef'),('schema-session-001','session','windows','1',NULL,NULL,NULL,NULL),('schema-sescrash-01','session_crash','windows','1',NULL,NULL,NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`INSERT INTO admin_events(id,kind,platform,version) VALUES('schema-bad-kind-01','other','windows','1')`,
		`INSERT INTO admin_events(id,kind,platform,version,install_id) VALUES('schema-bad-inst-01','active','windows','1','short')`,
		`INSERT INTO admin_events(id,kind,platform,version,signature) VALUES('schema-bad-sig-001','crash','windows','1','XYZ')`,
	} {
		if _, err := db.pool.Exec(ctx, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
