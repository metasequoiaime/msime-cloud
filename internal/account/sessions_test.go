package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDescribeSessionAgent(t *testing.T) {
	for _, tc := range []struct{ agent, platform, name, version string }{
		{"msime-android/1.0.0 (Pixel 8; Android 15; edition=full)", "android", "Pixel 8", "1.0.0"},
		{"msime-android/1.2.3-beta.1 (SM-S918B; Android 14; edition=wubi)", "android", "SM-S918B", "1.2.3-beta.1"},
		{"msime-ios/2.0 (iPhone15,2; iOS 18.0)", "ios", "iPhone15,2", "2.0"},
		{"msime-macos/0.9.1", "macos", "", "0.9.1"},
		{"msime-android/1.0.0 (edition=full)", "android", "", "1.0.0"},
		{"msime-android/1.0.0 (" + strings.Repeat("长", 65) + "; Android 15)", "android", "", "1.0.0"},
		{"msime-plan9/1.0.0 (Glenda)", "", "", ""},
		{"MSIME/Android", "android", "", ""},
		{"MSIME/iOS", "ios", "", ""},
		{"MSIME/Plan9", "", "", ""},
		{"MSIME-Client", "", "", ""},
		{"MSIME/42 CFNetwork/1568.100.1 Darwin/24.0.0", "", "", ""},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36", "web", "Chrome · macOS", ""},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1", "web", "Safari · iPhone", ""},
		{"Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36", "web", "Chrome · SM-S918B", ""},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0", "web", "Edge · Windows", ""},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "", "", ""},
		{"okhttp/4.12.0", "", "", ""},
		{"curl/8.7.1", "", "", ""},
		{"", "", "", ""},
	} {
		platform, name, version := describeSessionAgent(tc.agent)
		if platform != tc.platform || name != tc.name || version != tc.version {
			t.Errorf("%q: got %q %q %q, want %q %q %q", tc.agent, platform, name, version, tc.platform, tc.name, tc.version)
		}
	}
}

// 我的设备：只列出本人未撤销未过期的会话；撤销只对本人的会话生效（IDOR），别人的会话、已撤销和格式不对的 ID 一律 404，不泄露是否存在。
func TestUserSessionsListAndRevokeIsolation(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	login := func(identity Identity, agent string) Tokens {
		t.Helper()
		id := randomToken()
		c := Challenge{IDHash: hash(id), Provider: identity.Provider, Subject: identity.Subject}
		if err := db.PutChallenge(ctx, c); err != nil {
			t.Fatal(err)
		}
		v, err := db.completeWith(withSessionUserAgent(ctx, agent), c, identity, nil)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	phone := login(Identity{"email", "devices@example.test"}, "msime-android/1.0.0 (Pixel 8; Android 15; edition=full)")
	laptop := login(Identity{"email", "devices@example.test"}, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36")
	legacy := login(Identity{"email", "devices@example.test"}, "MSIME/Android")
	expired := login(Identity{"email", "devices@example.test"}, "msime-android/0.9.0 (Old; Android 13)")
	stranger := login(Identity{"email", "stranger@example.test"}, "msime-ios/2.0 (iPhone; iOS 18)")
	if _, err := db.pool.Exec(ctx, `UPDATE auth_sessions SET expires_at=now()-interval '1 second' WHERE access_hash=$1`, hash(expired.AccessToken)); err != nil {
		t.Fatal(err)
	}
	list := func(token string) []UserSession {
		t.Helper()
		w := apiRequest(t, mux, "GET", "/v1/users/me/sessions", "", token, 200)
		var body struct {
			Sessions []UserSession `json:"sessions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Sessions
	}
	apiRequest(t, mux, "GET", "/v1/users/me/sessions", "", "", 401)
	sessions := list(phone.AccessToken)
	if len(sessions) != 3 {
		t.Fatalf("expected the three live sessions, got %+v", sessions)
	}
	byName := map[string]UserSession{}
	current := 0
	for _, s := range sessions {
		byName[s.Platform+"|"+s.Name] = s
		if s.Current {
			current++
		}
		if len(s.ID) != 64 || s.CreatedAt.IsZero() || s.LastActive.Before(s.CreatedAt) {
			t.Fatalf("malformed session %+v", s)
		}
	}
	if current != 1 || !byName["android|Pixel 8"].Current || byName["android|Pixel 8"].AppVersion != "1.0.0" {
		t.Fatalf("current or parsed session wrong: %+v", sessions)
	}
	if _, ok := byName["web|Chrome · macOS"]; !ok {
		t.Fatalf("browser session missing: %+v", sessions)
	}
	if s, ok := byName["android|"]; !ok || s.AppVersion != "" {
		t.Fatalf("legacy session missing: %+v", sessions)
	}
	// 另一个用户看不到这些会话，也撤销不了它们。
	strangerSessions := list(stranger.AccessToken)
	if len(strangerSessions) != 1 || strangerSessions[0].Platform != "ios" {
		t.Fatalf("stranger saw other sessions: %+v", strangerSessions)
	}
	laptopID := byName["web|Chrome · macOS"].ID
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+laptopID, "", stranger.AccessToken, 404)
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+strangerSessions[0].ID, "", phone.AccessToken, 404)
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/not-a-session", "", phone.AccessToken, 404)
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+strings.ToUpper(laptopID), "", phone.AccessToken, 404)
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+laptopID, "", "", 401)
	apiRequest(t, mux, "GET", "/v1/users/me", "", laptop.AccessToken, 200)
	apiRequest(t, mux, "GET", "/v1/users/me", "", stranger.AccessToken, 200)
	// 撤销本人的另一台设备：它的访问令牌和刷新令牌立即失效，再撤销一次是 404。
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+laptopID, "", phone.AccessToken, 204)
	apiRequest(t, mux, "GET", "/v1/users/me", "", laptop.AccessToken, 401)
	if _, err := db.Refresh(ctx, laptop.RefreshToken); err == nil {
		t.Fatal("revoked session refreshed")
	}
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+laptopID, "", phone.AccessToken, 404)
	if got := list(phone.AccessToken); len(got) != 2 {
		t.Fatalf("revoked session still listed: %+v", got)
	}
	// 撤销当前会话等于退出登录。
	currentID := byName["android|Pixel 8"].ID
	apiRequest(t, mux, "DELETE", "/v1/users/me/sessions/"+currentID, "", phone.AccessToken, 204)
	apiRequest(t, mux, "GET", "/v1/users/me/sessions", "", phone.AccessToken, 401)
	if got := list(legacy.AccessToken); len(got) != 1 || !got[0].Current {
		t.Fatalf("remaining session: %+v", got)
	}
	// 列表最多 50 条。
	for i := 0; i < maxListedSessions+2; i++ {
		login(Identity{"email", "many@example.test"}, "msime-android/1.0.0 (Pixel; Android 15)")
	}
	many := login(Identity{"email", "many@example.test"}, "msime-android/1.0.0 (Newest; Android 15)")
	if got := list(many.AccessToken); len(got) != maxListedSessions || got[0].Name != "Newest" || !got[0].Current {
		t.Fatalf("list not bounded or not newest first: %d", len(got))
	}
}

// readOnlyService 返回连接只读事务的服务：鉴权照常读得到会话，写入一律失败，用来覆盖写库失败的分支。
func readOnlyService(t *testing.T, db *Store) *Service {
	t.Helper()
	config := db.pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Service{store: &Store{pool: pool}}
}

func TestUserSessionsStorageFailure(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "sessions-failure@example.test"})
	// 只读库：列表照常，撤销失败返回 503 且不生效。直接调处理函数，绕开 Route 里同样要写库的按地址限流。
	readOnly := readOnlyService(t, db)
	sessions := httptest.NewRecorder()
	readOnly.sessions(sessions, jsonRequest("GET", "/v1/users/me/sessions", "", user.AccessToken))
	var body struct {
		Sessions []UserSession `json:"sessions"`
	}
	if err := json.Unmarshal(sessions.Body.Bytes(), &body); err != nil || sessions.Code != 200 || len(body.Sessions) != 1 {
		t.Fatal(err, sessions.Body.String())
	}
	w := httptest.NewRecorder()
	r := jsonRequest("DELETE", "/v1/users/me/sessions/"+body.Sessions[0].ID, "", user.AccessToken)
	r.SetPathValue("id", body.Sessions[0].ID)
	readOnly.revokeSession(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "auth_unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := db.Authenticate(context.Background(), user.AccessToken); err != nil {
		t.Fatal("failed revoke took effect", err)
	}
	// 数据库关闭后列表返回 503。
	closed := &Service{store: db}
	db.Close()
	recorder := httptest.NewRecorder()
	closed.sessions(recorder, jsonRequest("GET", "/v1/users/me/sessions", "", user.AccessToken))
	if recorder.Code != 503 {
		t.Fatal(recorder.Code)
	}
}
