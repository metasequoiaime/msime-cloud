package server

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// adminRBACFixture is an admin host with an owner, a reviewer member, a read-only member with a personal access token, and the legacy token.
func adminRBACFixture(t *testing.T) (*Server, *adminMemoryStore) {
	t.Helper()
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", token: strings.Repeat("a", 40), tokenHash: sha256.Sum256([]byte(strings.Repeat("a", 40))), Google: AdminGoogleConfig{AllowedEmails: []string{"owner@example.test"}}}
	s.adminGoogle = &adminGoogleAuth{}
	store := &adminMemoryStore{
		sessions: map[string]account.AdminIdentity{
			strings.Repeat("o", 64): {Subject: "owner", Email: "owner@example.test"},
			strings.Repeat("r", 64): {Subject: "reviewer", Email: "reviewer@example.test"},
		},
		allowed: map[string]bool{"reviewer@example.test": true, "readonly@example.test": true},
		roles: map[string]adminMemoryRole{
			"reviewer@example.test": {"reviewer", []string{account.PermReviewDictPR, account.PermReviewCommunity, account.PermTriageIssues}},
			"readonly@example.test": {"readonly", []string{account.PermViewCloudUsage}},
		},
		tokens: map[string]account.AdminIdentity{account.AdminTokenPrefix + "readonly": {Subject: "readonly", Email: "readonly@example.test"}},
	}
	s.adminStore = store
	return s, store
}

func adminRequest(s *Server, method, path, cookie, bearer string) *http.Request {
	r := httptest.NewRequest(method, "https://admin.msime.app"+path, nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: cookie})
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

// Each identity reaches the handlers with its own role and permissions in the context.
func TestAdminAccessPerIdentity(t *testing.T) {
	s, store := adminRBACFixture(t)
	all := account.AllAdminPermissions()
	for _, tc := range []struct {
		name, cookie, bearer, actor, role string
		perms                             []string
		owner                             bool
	}{
		{"owner", strings.Repeat("o", 64), "", "google:owner:owner@example.test", account.RoleMaintainer, all, true},
		{"member", strings.Repeat("r", 64), "", "google:reviewer:reviewer@example.test", "reviewer", []string{account.PermReviewDictPR, account.PermReviewCommunity, account.PermTriageIssues}, false},
		{"legacy token", "", strings.Repeat("a", 40), "legacy-token", account.RoleMaintainer, all[:len(all)-1], false},
		{"personal access token", "", account.AdminTokenPrefix + "readonly", "pat:readonly@example.test", "readonly", []string{account.PermViewCloudUsage}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := adminRequest(s, "GET", "/api/shell", tc.cookie, tc.bearer)
			actor, email, err := s.adminIdentity(r)
			if err != nil {
				t.Fatal(err)
			}
			access, err := s.adminAccess(r.Context(), actor, email)
			if err != nil || access.Actor != tc.actor || access.Role != tc.role || !slices.Equal(access.Permissions, tc.perms) || access.Owner != tc.owner {
				t.Fatalf("%+v %v", access, err)
			}
		})
	}
	if slices.Contains(all[:len(all)-1], account.PermManagePermissions) {
		t.Fatal("the legacy token must not manage permissions")
	}
	// Membership is re-checked on every request, for personal access tokens too.
	store.allowed["readonly@example.test"] = false
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", "", account.AdminTokenPrefix+"readonly"))
	if w.Code != 401 {
		t.Fatal("disabled member's token accepted", w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", "", account.AdminTokenPrefix+"unknown"))
	if w.Code != 401 {
		t.Fatal("unknown token accepted", w.Code)
	}
	// Only owners manage admins, whatever their other permissions.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/admins", "", strings.Repeat("a", 40)))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "owner_required") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAdminRequirePerm(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if requirePerm(w, r, account.PermTriggerRelease) {
			respond(w, 200, map[string]bool{"ok": true})
		}
	}
	for _, tc := range []struct {
		perms  []string
		status int
	}{{[]string{account.PermTriggerRelease}, 200}, {[]string{account.PermReviewDictPR}, 403}, {nil, 403}} {
		r := httptest.NewRequest("POST", "/api/releases/windows/trigger", nil)
		r = r.WithContext(account.WithAdminAccess(r.Context(), account.AdminAccess{Actor: "x", Permissions: tc.perms}))
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != tc.status || (tc.status == 403 && !strings.Contains(w.Body.String(), "permission_denied")) {
			t.Fatal(tc.perms, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("POST", "/api/releases/windows/trigger", nil))
	if w.Code != 403 {
		t.Fatal("a request without admin access passed", w.Code)
	}
}

// 每个管理员单独计数，某个后台的密集请求不能占用其他管理员的额度。
func TestAdminRateLimitPerActor(t *testing.T) {
	s, _ := adminRBACFixture(t)
	// 夹具的监听器留在虚拟时钟外；下列请求直接调用处理函数，不经过网络。
	// 请求循环期间时间固定，避免慢机器在第 301 次请求前补充令牌。
	synctest.Test(t, func(t *testing.T) {
		for i := 0; i < adminRateLimit; i++ {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", "", strings.Repeat("a", 40)))
			if w.Code != 200 {
				t.Fatal(i, w.Code)
			}
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", "", strings.Repeat("a", 40)))
		if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
			t.Fatal("the legacy token exceeded its budget", w.Code)
		}
		for _, cookie := range []string{strings.Repeat("o", 64), strings.Repeat("r", 64)} {
			w = httptest.NewRecorder()
			s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", cookie, ""))
			if w.Code != 200 {
				t.Fatal("another admin was limited by the legacy token's traffic", w.Code)
			}
		}
		// 显式推进一个令牌的补充间隔，核对恢复一次请求后仍然限流。
		time.Sleep(time.Minute / adminRateLimit)
		w = httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", "", strings.Repeat("a", 40)))
		if w.Code != 200 {
			t.Fatal("补充一个令牌后请求未恢复", w.Code)
		}
		w = httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", "", strings.Repeat("a", 40)))
		if w.Code != 429 {
			t.Fatal("补充一个令牌后允许了多次请求", w.Code)
		}
	})
	// With a database the budget is counted in the limiter every replica shares, per actor under the admin scope; a spent budget is still 429 and a database failure 503 with Retry-After.
	limiter := &fakeLimiter{}
	s.limits = limiter
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", strings.Repeat("r", 64), ""))
	if w.Code != 200 || len(limiter.calls) != 1 || limiter.calls[0] != "admin|google:reviewer:reviewer@example.test" {
		t.Fatal("shared admin budget", w.Code, limiter.calls)
	}
	limiter.err = account.ErrLimited
	w = httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", strings.Repeat("r", 64), ""))
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "rate_limit_exceeded") {
		t.Fatal("spent shared budget", w.Code, w.Body.String())
	}
	limiter.err = errors.New("database down")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/system", strings.Repeat("r", 64), ""))
	if w.Code != 503 || w.Header().Get("Retry-After") != "30" || !strings.Contains(w.Body.String(), "admin_auth_unavailable") || strings.Contains(w.Body.String(), "database down") {
		t.Fatal("limiter failure", w.Code, w.Body.String())
	}
}

// Server-side console routes match before the account handlers, and a personal access token needs no Origin for a write.
func TestAdminServerRoutes(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		match         bool
	}{
		{"/api/dict-prs", "/api/dict-prs", true}, {"/api/dict-prs/", "/api/dict-prs/12/approve", true}, {"/api/dict-prs", "/api/dict-prsx", false},
		{"/api/crash-groups/{}/issue", "/api/crash-groups/0123456789abcdef/issue", true}, {"/api/crash-groups/{}/issue", "/api/crash-groups//issue", false}, {"/api/crash-groups/{}/issue", "/api/crash-groups/0123456789abcdef", false},
		{"/api/status", "/api/status/x", false},
	} {
		if adminRouteMatches(tc.pattern, tc.path) != tc.match {
			t.Error(tc)
		}
	}
	seen := map[string]bool{}
	for _, route := range adminServerRoutes {
		if seen[route.pattern] || route.handle == nil {
			t.Error("duplicate or empty route", route.pattern)
		}
		seen[route.pattern] = true
	}
	s, _ := adminRBACFixture(t)
	r := adminRequest(s, "POST", "/api/actions", "", account.AdminTokenPrefix+"readonly")
	if !s.adminMutationOrigin(httptest.NewRecorder(), r) {
		t.Fatal("a token-authenticated write was refused for lacking an Origin")
	}
	r = adminRequest(s, "POST", "/api/actions", strings.Repeat("r", 64), "")
	if s.adminMutationOrigin(httptest.NewRecorder(), r) {
		t.Fatal("a cookie-authenticated write passed without an Origin")
	}
}

// The admin host never serves a request without a context deadline and access, which handlers rely on.
func TestAdminAccessInContext(t *testing.T) {
	s, _ := adminRBACFixture(t)
	var got account.AdminAccess
	var deadline bool
	adminServerRoutes = append(adminServerRoutes, struct {
		pattern string
		handle  func(*Server, http.ResponseWriter, *http.Request)
	}{"/api/test-access", func(_ *Server, w http.ResponseWriter, r *http.Request) {
		got, _ = account.AdminAccessFrom(r.Context())
		_, deadline = r.Context().Deadline()
		respond(w, 200, map[string]bool{"ok": true})
	}})
	t.Cleanup(func() { adminServerRoutes = adminServerRoutes[:len(adminServerRoutes)-1] })
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/test-access", strings.Repeat("r", 64), ""))
	if w.Code != 200 || got.Email != "reviewer@example.test" || got.Role != "reviewer" || !deadline {
		t.Fatal(w.Code, got, deadline)
	}
}
