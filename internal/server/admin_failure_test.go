package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

type unavailableAdminStore struct {
	adminMemoryStore
	fail string
}

var errAdminStorage = errors.New("private database connection failure")

func (s *unavailableAdminStore) SaveAdminFlow(ctx context.Context, k string, v account.AdminLoginFlow) error {
	if s.fail == "save" {
		return errAdminStorage
	}
	return s.adminMemoryStore.SaveAdminFlow(ctx, k, v)
}
func (s *unavailableAdminStore) ConsumeAdminFlow(ctx context.Context, k string) (account.AdminLoginFlow, error) {
	if s.fail == "consume" {
		return account.AdminLoginFlow{}, errAdminStorage
	}
	return s.adminMemoryStore.ConsumeAdminFlow(ctx, k)
}
func (s *unavailableAdminStore) AdminSession(ctx context.Context, k string) (account.AdminIdentity, error) {
	if s.fail == "session" {
		return account.AdminIdentity{}, errAdminStorage
	}
	return s.adminMemoryStore.AdminSession(ctx, k)
}
func (s *unavailableAdminStore) DeleteAdminSession(ctx context.Context, tokens ...string) error {
	if s.fail == "delete" {
		return errAdminStorage
	}
	return s.adminMemoryStore.DeleteAdminSession(ctx, tokens...)
}
func (s *unavailableAdminStore) AdminEmailAllowed(ctx context.Context, email string) (bool, error) {
	if s.fail == "allowed" {
		return false, errAdminStorage
	}
	return s.adminMemoryStore.AdminEmailAllowed(ctx, email)
}

func TestAdminAuthenticationStorageFailuresAreSanitized(t *testing.T) {
	s := fixture(t, nil)
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", Google: AdminGoogleConfig{ClientID: "test", RedirectURI: "https://admin.msime.app" + adminCallbackPath}}
	s.initAdminGoogle()
	store := &unavailableAdminStore{adminMemoryStore: adminMemoryStore{flows: map[string]account.AdminLoginFlow{}, sessions: map[string]account.AdminIdentity{"session": {Subject: "person", Email: "member@example.test"}}}}
	s.adminStore = store
	for _, tc := range []struct {
		failure, method, path string
		flow                  bool
	}{
		{"save", "GET", "/api/auth/google/start", false},
		{"session", "GET", "/api/auth/session", false},
		{"allowed", "GET", "/api/auth/session", false},
		{"delete", "POST", "/api/auth/logout", false},
		{"consume", "GET", adminCallbackPath + "?state=" + strings.Repeat("a", 64) + "&code=code", true},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			store.fail = tc.failure
			r := httptest.NewRequest(tc.method, "https://admin.msime.app"+tc.path, nil)
			r.RemoteAddr = "local-test"
			r.Header.Set("Origin", "https://admin.msime.app")
			value := "session"
			if tc.flow {
				value = strings.Repeat("a", 64)
			}
			r.AddCookie(&http.Cookie{Name: s.adminCookieName(tc.flow), Value: value})
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	store.fail = ""
	// Separate budgets protect all auth endpoints and the more expensive login start.
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("GET", "https://admin.msime.app/api/auth/google/start", nil)
		r.RemoteAddr = "login-rate"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 302 {
			t.Fatal(i, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "https://admin.msime.app/api/auth/google/start", nil)
	r.RemoteAddr = "login-rate"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
	for i := 0; i < 120; i++ {
		r := httptest.NewRequest("GET", "https://admin.msime.app/api/auth/session", nil)
		r.RemoteAddr = "auth-rate"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	r = httptest.NewRequest("GET", "https://admin.msime.app/api/auth/session", nil)
	r.RemoteAddr = "auth-rate"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatal(w.Code)
	}
}
