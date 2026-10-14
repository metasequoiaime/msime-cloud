package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 只统计成员查询，实际鉴权仍委托给 PostgreSQL，避免把查询次数回归写成存储桩的行为测试。
type adminMembershipTrace struct {
	adminAuthStore
	allowed int
	roles   int
}

func (trace *adminMembershipTrace) AdminEmailAllowed(ctx context.Context, email string) (bool, error) {
	trace.allowed++
	return trace.adminAuthStore.AdminEmailAllowed(ctx, email)
}

func (trace *adminMembershipTrace) AdminMemberRole(ctx context.Context, email string) (string, []string, error) {
	trace.roles++
	return trace.adminAuthStore.AdminMemberRole(ctx, email)
}

func TestAdminRequestsResolveMembershipOnce(t *testing.T) {
	db, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_ADMIN_GOOGLE_SECRET", "synthetic-secret")
	t.Setenv("TEST_ADMIN_MEMBERSHIP_TOKEN", strings.Repeat("a", 40))
	s, err := New(Config{
		Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin: AdminConfig{Enabled: true, Host: "admin.msime.app", TokenEnv: "TEST_ADMIN_MEMBERSHIP_TOKEN", Google: AdminGoogleConfig{
			ClientID: "synthetic", SecretEnv: "TEST_ADMIN_GOOGLE_SECRET", AllowedEmails: []string{"owner@example.test"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseAccounts()
	defer s.Close()
	members := pgx.Identifier{schema, "admin_members"}.Sanitize()
	if _, err := db.Exec(t.Context(), "INSERT INTO "+members+"(email,role) VALUES('member@example.test','reviewer')"); err != nil {
		t.Fatal(err)
	}
	member, err := s.accounts.CreateAdminSession(t.Context(), account.AdminIdentity{Subject: "member", Email: "member@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.accounts.CreateAdminSession(t.Context(), account.AdminIdentity{Subject: "owner", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	pat := account.AdminTokenPrefix + strings.Repeat("b", 64)
	digest := sha256.Sum256([]byte(pat))
	if _, err := db.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{schema, "admin_tokens"}.Sanitize()+"(hash,email,last4) VALUES($1,'member@example.test','bbbb')", hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	trace := &adminMembershipTrace{adminAuthStore: s.adminStore}
	s.adminStore = trace
	limiter := &fakeLimiter{}
	s.limits = limiter
	check := func(t *testing.T, path, cookie, bearer string, status, allowed, roles int) *httptest.ResponseRecorder {
		t.Helper()
		trace.allowed, trace.roles = 0, 0
		w := httptest.NewRecorder()
		s.ServeHTTP(w, adminRequest(s, "GET", path, cookie, bearer))
		if w.Code != status || trace.allowed != allowed || trace.roles != roles {
			t.Errorf("%s: status=%d, allowed=%d, roles=%d; want %d/%d/%d; %s", path, w.Code, trace.allowed, trace.roles, status, allowed, roles, w.Body.String())
		}
		return w
	}
	for _, tc := range []struct {
		name, cookie, bearer string
		roles                int
	}{
		{"member cookie", member, "", 1},
		{"member bearer session", "", member, 1},
		{"member personal token", "", pat, 1},
		{"owner cookie", owner, "", 0},
		{"owner bearer session", "", owner, 0},
		{"legacy token", "", strings.Repeat("a", 40), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check(t, "/api/system", tc.cookie, tc.bearer, 200, 0, tc.roles)
		})
	}
	// 会话状态只需要启用状态，不应为了这一接口额外读取权限矩阵。
	w := check(t, "/api/auth/session", member, "", 200, 1, 0)
	var session struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil || !session.Authenticated {
		t.Fatal("有效成员的会话状态错误：", w.Body.String(), err)
	}
	check(t, "/api/system", "", account.AdminTokenPrefix+strings.Repeat("c", 64), 401, 0, 0)
	check(t, "/api/system", "", "unknown", 401, 0, 0)
	// 真实数据库里的改角色、改权限、停用和删除必须影响原凭据的下一次请求。
	check(t, "/api/cloud", member, "", 403, 0, 1)
	if _, err := db.Exec(t.Context(), "UPDATE "+members+" SET role='readonly' WHERE email='member@example.test'"); err != nil {
		t.Fatal(err)
	}
	check(t, "/api/cloud", "", pat, 200, 0, 1)
	permissions := pgx.Identifier{schema, "admin_role_permissions"}.Sanitize()
	if _, err := db.Exec(t.Context(), "DELETE FROM "+permissions+" WHERE role='readonly' AND permission='view_cloud_usage'"); err != nil {
		t.Fatal(err)
	}
	check(t, "/api/cloud", "", pat, 403, 0, 1)
	check(t, "/api/system", member, "", 200, 0, 1)
	if _, err := db.Exec(t.Context(), "UPDATE "+members+" SET enabled=false WHERE email='member@example.test'"); err != nil {
		t.Fatal(err)
	}
	for _, credential := range []struct{ cookie, bearer string }{{member, ""}, {"", pat}} {
		before := len(limiter.calls)
		check(t, "/api/system", credential.cookie, credential.bearer, 401, 0, 1)
		if len(limiter.calls) != before {
			t.Error("无效成员仍扣减后台请求额度")
		}
	}
	if _, err := db.Exec(t.Context(), "DELETE FROM "+members+" WHERE email='member@example.test'"); err != nil {
		t.Fatal(err)
	}
	check(t, "/api/system", member, "", 401, 0, 1)
	check(t, "/api/system", "", pat, 401, 0, 1)
	if _, err := db.Exec(t.Context(), "INSERT INTO "+members+"(email,role) VALUES('member@example.test','reviewer')"); err != nil {
		t.Fatal(err)
	}
	// 模拟成员查询的存储故障：必须拒绝访问，且不把表名和数据库错误透传给调用方。
	if _, err := db.Exec(t.Context(), "ALTER TABLE "+members+" RENAME TO unavailable_members"); err != nil {
		t.Fatal(err)
	}
	w = check(t, "/api/system", member, "", 503, 0, 1)
	if !strings.Contains(w.Body.String(), "admin_auth_unavailable") || strings.Contains(w.Body.String(), "admin_members") || strings.Contains(w.Body.String(), "SQLSTATE") {
		t.Fatal("鉴权存储错误未脱敏：", w.Body.String())
	}
}
