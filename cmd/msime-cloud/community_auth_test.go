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

func TestPublicHEADCallsOmitIdentity(t *testing.T) {
	for _, source := range []string{"environment", "saved", "expired"} {
		for _, path := range []string{"/v1/notices", "/v1/community/stats", "/v1/community/word-submissions?limit=1"} {
			t.Run(source+path, func(t *testing.T) {
				h := newHarness(t)
				var calls, refreshes atomic.Int32
				mux := http.NewServeMux()
				plainPath, _, _ := strings.Cut(path, "?")
				mux.HandleFunc("GET "+plainPath, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodHead || r.URL.RequestURI() != path || r.Header.Get("Authorization") != "" {
						t.Errorf("匿名 HEAD 请求错误：method=%s path=%s auth=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
					}
					w.Write([]byte("合成响应正文"))
				})
				mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
					refreshes.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
				})
				server := httptest.NewServer(mux)
				t.Cleanup(server.Close)
				h.env["MSIME_CLOUD_URL"] = server.URL
				current := session{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: h.now.Add(time.Hour)}
				if source == "expired" {
					current.ExpiresAt = h.now.Add(-time.Minute)
				}
				if source == "environment" {
					h.env["MSIME_CLOUD_TOKEN"] = current.AccessToken
				} else if err := (store{dir: h.dir}).put(server.URL, &current); err != nil {
					t.Fatal(err)
				}
				code, out, errText := h.run("", "call", "HEAD", path)
				if code != 0 || out != "" || errText != "" {
					t.Errorf("HEAD 调用：code=%d out=%q err=%s", code, out, errText)
				}
				if calls.Load() != 1 || refreshes.Load() != 0 {
					t.Errorf("匿名接口调用次数=%d 刷新次数=%d", calls.Load(), refreshes.Load())
				}
				if source != "environment" {
					saved, ok, err := (store{dir: h.dir}).get(server.URL)
					if err != nil || !ok || saved.AccessToken != current.AccessToken || saved.RefreshToken != current.RefreshToken || !saved.ExpiresAt.Equal(current.ExpiresAt) {
						t.Errorf("匿名 HEAD 改动了保存的会话：ok=%v err=%v", ok, err)
					}
				}
			})
		}
	}
}

func TestIdentityHEADCallsUseGETAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, path, source, wantAuth string
		refreshStatus, wantCode      int
		wantCalls, wantRefreshes     int32
	}{
		{"optional-environment", "/v1/community/skins/synthetic", "environment", "Bearer synthetic-access", 0, 0, 1, 0},
		{"optional-saved", "/v1/community/skins", "saved", "Bearer synthetic-access", 0, 0, 1, 0},
		{"optional-refreshed", "/v1/community/skins", "expired", "Bearer refreshed-access", 200, 0, 1, 1},
		{"optional-revoked", "/v1/community/skins", "expired", "", 401, 0, 1, 1},
		{"optional-unavailable", "/v1/community/skins", "expired", "", 503, 1, 0, 1},
		{"required-environment", "/v1/users/me", "environment", "Bearer synthetic-access", 0, 0, 1, 0},
		{"required-refreshed", "/v1/users/me", "expired", "Bearer refreshed-access", 200, 0, 1, 1},
		{"required-revoked", "/v1/users/me", "expired", "", 401, 1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var calls, refreshes atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("GET "+tc.path, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodHead || r.Header.Get("Authorization") != tc.wantAuth {
					t.Errorf("身份 HEAD 请求错误：method=%s auth=%q，期望 %q", r.Method, r.Header.Get("Authorization"), tc.wantAuth)
				}
				w.Write([]byte("合成响应正文"))
			})
			mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				w.WriteHeader(tc.refreshStatus)
				if tc.refreshStatus == http.StatusOK {
					json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed-access", "refresh_token": "refreshed-refresh", "token_type": "Bearer", "expires_in": 900})
				}
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			h.env["MSIME_CLOUD_URL"] = server.URL
			current := session{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: h.now.Add(time.Hour)}
			if tc.source == "expired" {
				current.ExpiresAt = h.now.Add(-time.Minute)
			}
			if tc.source == "environment" {
				h.env["MSIME_CLOUD_TOKEN"] = current.AccessToken
			} else if err := (store{dir: h.dir}).put(server.URL, &current); err != nil {
				t.Fatal(err)
			}
			code, out, errText := h.run("", "call", "HEAD", tc.path)
			if code != tc.wantCode || out != "" || strings.Contains(errText, "not in the API description") {
				t.Errorf("HEAD 调用：code=%d，期望 %d out=%q err=%s", code, tc.wantCode, out, errText)
			}
			if calls.Load() != tc.wantCalls || refreshes.Load() != tc.wantRefreshes {
				t.Errorf("身份接口调用次数=%d，期望 %d；刷新次数=%d，期望 %d", calls.Load(), tc.wantCalls, refreshes.Load(), tc.wantRefreshes)
			}
			if tc.refreshStatus == http.StatusUnauthorized {
				if _, ok, err := (store{dir: h.dir}).get(server.URL); err != nil || ok {
					t.Errorf("撤销后的会话仍被保留：ok=%v err=%v", ok, err)
				}
			}
			if tc.refreshStatus == http.StatusServiceUnavailable {
				if saved, ok, err := (store{dir: h.dir}).get(server.URL); err != nil || !ok || saved.AccessToken != current.AccessToken {
					t.Errorf("临时刷新失败丢失会话：ok=%v err=%v", ok, err)
				}
			}
		})
	}
}

func TestEscapedPublicCallsOmitIdentity(t *testing.T) {
	for _, path := range []string{"/v1/%6eotices", "/%76%31/notices", "/v1/notices#ignored", "/v1/notices?limit=1#ignored"} {
		for _, source := range []string{"environment", "expired"} {
			t.Run(source+path, func(t *testing.T) {
				h := newHarness(t)
				var calls, refreshes atomic.Int32
				mux := http.NewServeMux()
				mux.HandleFunc("GET /v1/notices", func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					wantURI, _, _ := strings.Cut(path, "#")
					if r.Method != http.MethodGet || r.URL.RequestURI() != wantURI {
						t.Errorf("请求地址被改写：method=%s uri=%s，期望 %s", r.Method, r.URL.RequestURI(), wantURI)
					}
					if r.Header.Get("Authorization") != "" {
						t.Error("编码的匿名路径附带了凭据")
					}
					w.WriteHeader(http.StatusNoContent)
				})
				mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
					refreshes.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
				})
				server := httptest.NewServer(mux)
				t.Cleanup(server.Close)
				h.env["MSIME_CLOUD_URL"] = server.URL
				current := session{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: h.now.Add(-time.Minute)}
				if source == "environment" {
					h.env["MSIME_CLOUD_TOKEN"] = current.AccessToken
				} else if err := (store{dir: h.dir}).put(server.URL, &current); err != nil {
					t.Fatal(err)
				}
				code, out, errText := h.run("", "call", "GET", path)
				if code != 0 || out != "" || errText != "" {
					t.Errorf("编码路径调用：code=%d out=%q err=%s", code, out, errText)
				}
				if calls.Load() != 1 || refreshes.Load() != 0 {
					t.Errorf("匿名接口调用次数=%d 刷新次数=%d", calls.Load(), refreshes.Load())
				}
				if source == "expired" {
					if saved, ok, err := (store{dir: h.dir}).get(server.URL); err != nil || !ok || saved.AccessToken != current.AccessToken {
						t.Errorf("匿名调用改动了会话：ok=%v err=%v", ok, err)
					}
				}
			})
		}
	}
}

func TestEscapedCallsPreserveLiteralRoutePriority(t *testing.T) {
	for _, tc := range []struct {
		path      string
		wantCalls int32
		wantCode  int
	}{
		{"/v1/community/candidate-skins/s%79nc", 0, 1},
		{"/v1/community/candidate-skins/s%2Fync", 1, 0},
		{"/v1/community/candidate-skins/s%2579nc", 1, 0},
	} {
		t.Run(tc.path, func(t *testing.T) {
			h := newHarness(t)
			var calls, refreshes atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/community/candidate-skins/sync", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
			})
			mux.HandleFunc("GET /v1/community/candidate-skins/{id}", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			h.env["MSIME_CLOUD_URL"] = server.URL
			current := session{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: h.now.Add(-time.Minute)}
			if err := (store{dir: h.dir}).put(server.URL, &current); err != nil {
				t.Fatal(err)
			}
			code, _, errText := h.run("", "call", "GET", tc.path)
			if code != tc.wantCode || strings.Contains(errText, "not in the API description") {
				t.Errorf("编码路径调用：code=%d，期望 %d err=%s", code, tc.wantCode, errText)
			}
			if calls.Load() != tc.wantCalls || refreshes.Load() != 1 {
				t.Errorf("接口调用次数=%d，期望 %d；刷新次数=%d", calls.Load(), tc.wantCalls, refreshes.Load())
			}
		})
	}
}

func TestInvalidEscapedPathPreservesSession(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	h.now = h.now.Add(time.Hour)
	h.cloud.mu.Lock()
	before := len(h.cloud.seen)
	h.cloud.mu.Unlock()
	if code, _, errText := h.run("", "call", "GET", "/v1/%zz"); code != 2 || !strings.Contains(errText, "invalid URL escape") {
		t.Errorf("无效路径调用：code=%d err=%s", code, errText)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if len(h.cloud.seen) != before {
		t.Error("无效路径触发了 HTTP 请求或会话刷新")
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
