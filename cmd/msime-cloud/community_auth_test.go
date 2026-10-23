package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newCommunityHarness(t *testing.T) *harness {
	h := newHarness(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/community/") || r.URL.Path == "/v1/community/stats" || r.URL.Path == "/v1/community/word-submissions" {
			h.cloud.ServeHTTP(w, r)
			return
		}
		h.cloud.mu.Lock()
		defer h.cloud.mu.Unlock()
		viewer := h.cloud.access != "" && r.Header.Get("Authorization") == "Bearer "+h.cloud.access
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("scope") != "" && !viewer {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "user_session_required"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"owned": viewer})
	}))
	t.Cleanup(server.Close)
	h.url = server.URL
	h.env["MSIME_CLOUD_URL"] = server.URL
	return h
}

func TestCommunityCallsKeepOptionalIdentity(t *testing.T) {
	for _, source := range []string{"anonymous", "saved", "environment"} {
		t.Run(source, func(t *testing.T) {
			h := newCommunityHarness(t)
			if source != "anonymous" {
				h.signIn()
			}
			if source == "environment" {
				h.env["MSIME_CLOUD_TOKEN"] = h.cloud.access
				if err := (store{dir: h.dir}).put(h.url, nil); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []string{"skins", "plugins", "candidate-skins", "resources"} {
				for _, suffix := range []string{"", "/synthetic"} {
					path := "/v1/community/" + kind + suffix
					if code, out, errText := h.run("", "call", "GET", path); code != 0 || !strings.Contains(out, `"owned": `+strconv.FormatBool(source != "anonymous")) {
						t.Fatalf("%s: code=%d out=%s err=%s", path, code, out, errText)
					}
				}
				for _, scope := range []string{"mine", "saved"} {
					want := 0
					if source == "anonymous" {
						want = 1
					}
					if code, out, errText := h.run("", "call", "GET", "/v1/community/"+kind, "-q", "scope="+scope); code != want {
						t.Fatalf("%s scope=%s: code=%d out=%s err=%s", kind, scope, code, out, errText)
					}
				}
			}
			for _, call := range [][]string{
				{"GET", "/v1/community/candidate-skins/synthetic/preview"},
				{"POST", "/v1/community/candidate-skins/synthetic/download"},
			} {
				if code, out, errText := h.run("", append([]string{"call"}, call...)...); code != 0 || strings.Contains(out, `"owned": true`) != (source != "anonymous") {
					t.Fatalf("%v: code=%d out=%s err=%s", call, code, out, errText)
				}
			}
		})
	}
}

func TestCommunityExpiredSessionRefresh(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "revoked"}[revoked], func(t *testing.T) {
			h := newCommunityHarness(t)
			h.signIn()
			h.now = h.now.Add(16 * time.Minute)
			if revoked {
				h.cloud.mu.Lock()
				h.cloud.refresh = "revoked"
				h.cloud.access = ""
				h.cloud.mu.Unlock()
			}
			if code, out, errText := h.run("", "call", "GET", "/v1/community/skins"); code != 0 || strings.Contains(out, `"owned": true`) == revoked {
				t.Fatalf("公开浏览：code=%d out=%s err=%s", code, out, errText)
			}
			if h.cloud.refreshes != 1 {
				t.Fatalf("刷新次数=%d，期望 1", h.cloud.refreshes)
			}
			if revoked {
				if _, ok, err := (store{dir: h.dir}).get(h.url); err != nil || ok {
					t.Fatalf("失效会话仍被保留：ok=%v err=%v", ok, err)
				}
				if code, _, _ := h.run("", "call", "GET", "/v1/community/skins?scope=mine"); code != 1 {
					t.Fatal("失效会话不能查看自己的作品")
				}
			}
		})
	}
}

func TestCommunityPublicOnlyCallsOmitIdentity(t *testing.T) {
	h := newCommunityHarness(t)
	h.signIn()
	h.env["MSIME_CLOUD_TOKEN"] = h.cloud.access
	for _, path := range []string{"/v1/community/stats", "/v1/community/word-submissions", "/v1/notices"} {
		h.run("", "call", "GET", path)
		h.cloud.mu.Lock()
		last := h.cloud.seen[len(h.cloud.seen)-1]
		h.cloud.mu.Unlock()
		if last != "GET "+path+" auth=" {
			t.Fatalf("纯公开接口附带了身份：%s", last)
		}
	}
}

func TestCommunityRefreshFailurePreservesIdentity(t *testing.T) {
	h := newHarness(t)
	var communityCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/refresh" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		communityCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	h.env["MSIME_CLOUD_URL"] = server.URL
	current := session{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: h.now.Add(-time.Minute)}
	if err := (store{dir: h.dir}).put(server.URL, &current); err != nil {
		t.Fatal(err)
	}
	if code, _, errText := h.run("", "call", "GET", "/v1/community/skins"); code != 1 || !strings.Contains(errText, "503") {
		t.Fatalf("刷新失败：code=%d err=%s", code, errText)
	}
	if communityCalled.Load() {
		t.Fatal("刷新失败后不应悄悄匿名访问")
	}
	if saved, ok, err := (store{dir: h.dir}).get(server.URL); err != nil || !ok || saved.AccessToken != current.AccessToken {
		t.Fatalf("临时刷新失败丢失会话：ok=%v err=%v", ok, err)
	}
}

func TestCommunityCatalogSecurity(t *testing.T) {
	operations, err := catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, auth string }{
		{"GET", "/v1/community/skins", "optional-user"},
		{"GET", "/v1/community/candidate-skins/synthetic/preview", "optional-user"},
		{"POST", "/v1/community/candidate-skins/synthetic/download", "optional-user"},
		{"GET", "/v1/community/candidate-skins/sync", "user"},
		{"POST", "/v1/community/skins", "user"},
		{"POST", "/v1/community/skins/synthetic/download", "user"},
		{"POST", "/v1/community/plugins/synthetic/download", "user"},
		{"GET", "/v1/community/stats", "none"},
	} {
		if op, ok := find(operations, tc.method, tc.path); !ok || op.Auth != tc.auth {
			t.Fatalf("%s %s：auth=%s，期望 %s", tc.method, tc.path, op.Auth, tc.auth)
		}
	}
}

func TestOptionalSecurityRequirements(t *testing.T) {
	for _, tc := range []struct{ security, want string }{
		{`[]`, "none"},
		{`[{}]`, "none"},
		{`[{"userSession":[]}]`, "user"},
		{`[{},{"userSession":[]}]`, "optional-user"},
		{`[{"userSession":[]},{}]`, "optional-user"},
		{`[{"deviceToken":[]},{"userSession":[]}]`, "device-or-user"},
	} {
		if got := authOf(json.RawMessage(tc.security)); got != tc.want {
			t.Fatalf("security=%s: auth=%s，期望 %s", tc.security, got, tc.want)
		}
	}
}
