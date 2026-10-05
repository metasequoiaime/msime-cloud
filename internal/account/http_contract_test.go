package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func apiRequest(t *testing.T, handler http.Handler, method, path, body, token string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}

func TestAccountHTTPProfileRefreshLogoutDelete(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	first := complete(t, db, Identity{"email", "profile@example.test"})
	other := complete(t, db, Identity{"email", "other@example.test"})
	apiRequest(t, mux, "PATCH", "/v1/users/me", `{"display_name":"  测试昵称  "}`, first.AccessToken, 204)
	profile := apiRequest(t, mux, "GET", "/v1/users/me", "", first.AccessToken, 200)
	var body struct {
		User       User       `json:"user"`
		Identities []Identity `json:"identities"`
	}
	if err := json.Unmarshal(profile.Body.Bytes(), &body); err != nil || body.User.ID != first.User.ID || body.User.DisplayName != "测试昵称" || len(body.Identities) != 1 {
		t.Fatal(profile.Body.String(), err)
	}
	badName, _ := json.Marshal(map[string]string{"display_name": strings.Repeat("长", 65)})
	apiRequest(t, mux, "PATCH", "/v1/users/me", string(badName), first.AccessToken, 400)
	refreshBody, _ := json.Marshal(map[string]string{"refresh_token": first.RefreshToken})
	w := apiRequest(t, mux, "POST", "/v1/auth/refresh", string(refreshBody), "", 200)
	var refreshed Tokens
	if err := json.Unmarshal(w.Body.Bytes(), &refreshed); err != nil || refreshed.User.ID != first.User.ID || refreshed.RefreshToken == first.RefreshToken || refreshed.AccessToken == first.AccessToken {
		t.Fatal("tokens not rotated", err)
	}
	apiRequest(t, mux, "GET", "/v1/users/me", "", first.AccessToken, 401)
	apiRequest(t, mux, "GET", "/v1/users/me", "", refreshed.AccessToken, 200)
	// 轮换后 30 秒内再次提交旧令牌：409 refresh_superseded，会话保留。
	if w = apiRequest(t, mux, "POST", "/v1/auth/refresh", string(refreshBody), "", 409); !strings.Contains(w.Body.String(), `"code":"refresh_superseded"`) {
		t.Fatal(w.Body.String())
	}
	apiRequest(t, mux, "GET", "/v1/users/me", "", refreshed.AccessToken, 200)
	if _, err := db.pool.Exec(t.Context(), "UPDATE auth_used_refresh SET used_at=now()-interval '31 seconds'"); err != nil {
		t.Fatal(err)
	}
	apiRequest(t, mux, "POST", "/v1/auth/refresh", string(refreshBody), "", 401)
	apiRequest(t, mux, "GET", "/v1/users/me", "", refreshed.AccessToken, 401)
	second := complete(t, db, Identity{"email", "profile@example.test"})
	third := complete(t, db, Identity{"email", "profile@example.test"})
	apiRequest(t, mux, "POST", "/v1/auth/logout", `{"all":false}`, second.AccessToken, 204)
	apiRequest(t, mux, "GET", "/v1/users/me", "", second.AccessToken, 401)
	apiRequest(t, mux, "GET", "/v1/users/me", "", third.AccessToken, 200)
	fourth := complete(t, db, Identity{"email", "profile@example.test"})
	apiRequest(t, mux, "POST", "/v1/auth/logout", `{"all":true}`, third.AccessToken, 204)
	apiRequest(t, mux, "GET", "/v1/users/me", "", fourth.AccessToken, 401)
	fresh := complete(t, db, Identity{"email", "profile@example.test"})
	apiRequest(t, mux, "DELETE", "/v1/users/me", "", fresh.AccessToken, 204)
	apiRequest(t, mux, "GET", "/v1/users/me", "", fresh.AccessToken, 401)
	apiRequest(t, mux, "GET", "/v1/users/me", "", other.AccessToken, 200)
	apiRequest(t, mux, "POST", "/v1/auth/refresh", `{"refresh_token":"missing"}`, "", 401)
}

// Walk the published operations so new account APIs cannot silently miss their
// authentication, disabled-service and router method contracts.
func TestEveryAccountRouteAuthenticationAndDisabledService(t *testing.T) {
	raw, err := os.ReadFile("../server/swagger/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	disabled := http.NewServeMux()
	Mount(disabled, nil)
	// The server package registers the report route with Route instead of Mount; register it the same way so it meets the same contract.
	mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
	disabled.HandleFunc("POST /v1/community/reports", Route(nil, "POST /v1/community/reports", (*Service).CommunityReport))
	mux.HandleFunc("POST "+TelemetryPath, Route(a, "POST "+TelemetryPath, (*Service).Telemetry))
	disabled.HandleFunc("POST "+TelemetryPath, Route(nil, "POST "+TelemetryPath, (*Service).Telemetry))
	n := 0
	for path, methods := range spec.Paths {
		// The website word form is mounted by the server package and covered by its word submission tests.
		if !IsPath(path) || path == "/v1/community/word-submissions" {
			continue
		}
		concrete := strings.NewReplacer("{id}", "missing", "{kind}", "pinyin").Replace(path)
		for method := range methods {
			if method != "get" && method != "post" && method != "put" && method != "patch" && method != "delete" {
				continue
			}
			n++
			expected := 401
			// Community and website reads are public, except the author's own sync listing.
			if method == "get" && (strings.HasPrefix(path, "/v1/community/") || path == siteDownloadMirrorsPath) && path != "/v1/community/candidate-skins/sync" {
				if strings.Contains(path, "{id}") {
					expected = 404
				} else {
					expected = 200
				}
				if path == "/v1/community/resources" {
					concrete += "?kind=dictionary"
				}
			}
			// 遥测完全不需要凭据：空事件因内容不合法被拒绝，带不带令牌都一样。
			if path == TelemetryPath {
				expected = 400
			}
			// Apple 网页登录的三条路由在登录之前调用，本来就不要凭据。测试里没有配置 Apple，所以各自给出固定的回应：发起登录报服务未开启，回调返回说明登录请求无效的页面，用一次性授权码换令牌时授权码不存在。
			switch path {
			case "/v1/auth/apple/web":
				expected = 503
			case "/v1/auth/apple/callback":
				expected = 200
			case "/v1/auth/apple/web/login":
				expected = 400
			}
			t.Run(method+" "+path, func(t *testing.T) {
				verb := strings.ToUpper(method)
				apiRequest(t, disabled, verb, concrete, `{}`, "", 503)
				apiRequest(t, mux, "TRACE", concrete, `{}`, "", 405)
				if path == "/v1/auth/providers" || path == "/v1/auth/challenges" || path == "/v1/auth/login" || path == "/v1/auth/refresh" {
					return
				}
				for i, token := range []string{"", "invalid-client-token"} {
					// Distinct peers keep this routing test independent of the shared quota test.
					r := httptest.NewRequest(verb, concrete, strings.NewReader(`{}`))
					r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
					r.Header.Set("Content-Type", "application/json")
					if token != "" {
						r.Header.Set("Authorization", "Bearer "+token)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					if w.Code != expected {
						t.Fatalf("unauthenticated route returned %d: %s", w.Code, w.Body.String())
					}
				}
			})
		}
	}
	if n == 0 {
		t.Fatal("account route inventory empty")
	}
}

func TestProviderDiscoveryAndMalformedAuthenticationBodies(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db, config: Config{Google: GoogleConfig{ClientIDs: []string{"test-client"}}, Email: MailConfig{From: "test@example.test"}}}
	mux := http.NewServeMux()
	Mount(mux, a)
	w := apiRequest(t, mux, "GET", "/v1/auth/providers", "", "", 200)
	var response struct {
		Providers map[string]bool `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Providers) != 7 || !response.Providers["google"] || !response.Providers["email"] || response.Providers["phone"] || response.Providers["anonymous"] || response.Providers["apple_web"] {
		t.Fatal(w.Body.String(), err)
	}
	user := complete(t, db, Identity{"email", "malformed@example.test"})
	for _, path := range []string{"/v1/auth/challenges", "/v1/auth/login", "/v1/auth/refresh", "/v1/auth/logout"} {
		for _, body := range []string{`{`, `{} {}`, `{"unknown":true}`} {
			apiRequest(t, mux, "POST", path, body, user.AccessToken, 400)
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 415 {
			t.Fatal(path, w.Code)
		}
	}
	apiRequest(t, mux, "POST", "/v1/auth/challenges", `{"provider":"unknown"}`, "", 503)
	apiRequest(t, mux, "POST", "/v1/auth/login", `{"challenge_id":"missing","credential":"invalid"}`, "", 401)
}

func TestClipboardClearAndCommunityResourceDeleteHTTP(t *testing.T) {
	db := testStore(t)
	one := complete(t, db, Identity{"email", "clear-one@example.test"})
	two := complete(t, db, Identity{"email", "clear-two@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	for _, token := range []string{one.AccessToken, two.AccessToken} {
		apiRequest(t, mux, "PUT", "/v1/users/me/clipboard/settings", `{"enabled":true}`, token, 200)
		apiRequest(t, mux, "POST", "/v1/users/me/clipboard", `{"text":"retained by its owner"}`, token, 200)
	}
	apiRequest(t, mux, "DELETE", "/v1/users/me/clipboard", "", one.AccessToken, 204)
	var response struct {
		Items []json.RawMessage `json:"items"`
	}
	w := apiRequest(t, mux, "GET", "/v1/users/me/clipboard", "", one.AccessToken, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Items) != 0 {
		t.Fatal(w.Body.String(), err)
	}
	w = apiRequest(t, mux, "GET", "/v1/users/me/clipboard", "", two.AccessToken, 200)
	if !strings.Contains(w.Body.String(), "retained by its owner") {
		t.Fatal("clear crossed owner boundary")
	}
	apiRequest(t, mux, "DELETE", "/v1/users/me/clipboard", "", one.AccessToken, 204)
	const id = "ab334455-1234-1234-1234-123456789ddd"
	path := "/v1/community/resources/" + id
	apiRequest(t, mux, "POST", "/v1/community/resources", `{"id":"`+id+`","kind":"reply","name":"Delete fixture","content":{"prompt":"Hello"},"revision":0}`, one.AccessToken, 201)
	apiRequest(t, mux, "PUT", path+"/save", `{"saved":true}`, two.AccessToken, 200)
	apiRequest(t, mux, "PUT", path+"/rating", `{"stars":4}`, two.AccessToken, 200)
	apiRequest(t, mux, "DELETE", path, "", two.AccessToken, 404)
	apiRequest(t, mux, "DELETE", path, "", one.AccessToken, 200)
	apiRequest(t, mux, "GET", path, "", "", 404)
	apiRequest(t, mux, "DELETE", path, "", one.AccessToken, 404)
	var remaining int
	if err := db.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM community_resource_saves WHERE resource_id=$1)+(SELECT count(*) FROM community_resource_ratings WHERE resource_id=$1)`, id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("orphaned community records", remaining, err)
	}
}

func TestEveryAccountJSONBodyRejectsMalformedInput(t *testing.T) {
	raw, err := os.ReadFile("../server/swagger/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	db := testStore(t)
	user := complete(t, db, Identity{"email", "body-contract@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	mux.HandleFunc("POST /v1/community/reports", Route(&Service{store: db}, "POST /v1/community/reports", (*Service).CommunityReport))
	mux.HandleFunc("POST "+TelemetryPath, Route(&Service{store: db}, "POST "+TelemetryPath, (*Service).Telemetry))
	count := 0
	for path, methods := range spec.Paths {
		// The website word form is mounted by the server package; its malformed-body cases are in TestWordSubmissionValidation there.
		if !IsPath(path) || path == "/v1/community/word-submissions" {
			continue
		}
		path = strings.NewReplacer("{id}", "missing", "{kind}", "pinyin").Replace(path)
		for method, rawOperation := range methods {
			if method != "post" && method != "put" && method != "patch" && method != "delete" {
				continue
			}
			var operation struct {
				RequestBody struct {
					Content map[string]json.RawMessage `json:"content"`
				} `json:"requestBody"`
			}
			if err := json.Unmarshal(rawOperation, &operation); err != nil {
				t.Fatal(err)
			}
			if operation.RequestBody.Content["application/json"] == nil {
				continue
			}
			count++
			t.Run(method+" "+path, func(t *testing.T) {
				// Every request comes from the same test address, and four per operation across all documented routes exceed the 120 per minute per-address limit, which this test does not exercise.
				if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_rates WHERE key LIKE 'ip:%'`); err != nil {
					t.Fatal(err)
				}
				for _, body := range []string{`{`, `{} {}`, `{"unexpected_contract_field":true}`} {
					apiRequest(t, mux, strings.ToUpper(method), path, body, user.AccessToken, 400)
				}
				r := httptest.NewRequest(strings.ToUpper(method), path, strings.NewReader(`{}`))
				r.Header.Set("Authorization", "Bearer "+user.AccessToken)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if w.Code != 415 {
					t.Fatal("media type accepted", w.Code, w.Body.String())
				}
			})
		}
	}
	if count == 0 {
		t.Fatal("no JSON request bodies checked")
	}
}

func TestClientAddress(t *testing.T) {
	request := func(remote string, headers map[string][]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for name, values := range headers {
			for _, v := range values {
				r.Header.Add(name, v)
			}
		}
		return r
	}
	spoofed := map[string][]string{"CF-Connecting-IP": {"203.0.113.9"}, "X-Forwarded-For": {"203.0.113.9"}}
	for _, tc := range []struct {
		header, remote string
		headers        map[string][]string
		want           string
	}{
		// 没有配置头时只认 TCP 对端，不管客户端带了什么。
		{"", "198.51.100.7:1", spoofed, "198.51.100.7"},
		{"", "[2001:db8:1:2:3:4:5:6]:1", nil, "2001:db8:1:2::/64"},
		{"", "[::ffff:198.51.100.7]:1", nil, "198.51.100.7"},
		{"", "not-an-address", nil, "not-an-address"},
		// X-Forwarded-For：取最后一项，即最近的代理追加的那一项。
		{"X-Forwarded-For", "10.0.0.1:1", map[string][]string{"X-Forwarded-For": {"192.0.2.1, 192.0.2.2", "192.0.2.3, 203.0.113.5"}}, "203.0.113.5"},
		{"X-Forwarded-For", "10.0.0.1:1", map[string][]string{"X-Forwarded-For": {"garbage"}}, "10.0.0.1"},
		{"CF-Connecting-IP", "10.0.0.1:1", map[string][]string{"Cf-Connecting-Ip": {"192.0.2.44"}}, "192.0.2.44"},
		{"CF-Connecting-IP", "10.0.0.1:1", map[string][]string{"CF-Connecting-IP": {"2001:db8:aa:bb:1::1"}}, "2001:db8:aa:bb::/64"},
		// 配置了头但请求里没有时，退回 TCP 对端。
		{"CF-Connecting-IP", "10.0.0.1:1", nil, "10.0.0.1"},
	} {
		if got := ClientAddress(request(tc.remote, tc.headers), tc.header, ""); got != tc.want {
			t.Errorf("ClientAddress(%s via %q) = %q, want %q", tc.remote, tc.header, got, tc.want)
		}
	}
	// 官网代理：只有密钥相符时才采用 X-MSIME-Client-IP，其他情况两个头都被忽略。
	secret := strings.Repeat("s", 40)
	proxied := func(proof, visitor string) map[string][]string {
		return map[string][]string{"CF-Connecting-IP": {"192.0.2.10"}, SiteProxyHeader: {proof}, SiteProxyClientIPHeader: {visitor}}
	}
	for _, tc := range []struct {
		name, secret string
		headers      map[string][]string
		want         string
	}{
		{"密钥相符", secret, proxied(secret, "203.0.113.77"), "203.0.113.77"},
		{"密钥相符的 IPv6 按 /64 归组", secret, proxied(secret, "2001:db8:5:6:7::1"), "2001:db8:5:6::/64"},
		{"密钥相符但地址不合法", secret, proxied(secret, "not-an-ip"), "192.0.2.10"},
		{"密钥相符但没带地址", secret, map[string][]string{"CF-Connecting-IP": {"192.0.2.10"}, SiteProxyHeader: {secret}}, "192.0.2.10"},
		{"密钥不符", secret, proxied(secret+"x", "203.0.113.77"), "192.0.2.10"},
		{"缺少密钥头", secret, map[string][]string{"CF-Connecting-IP": {"192.0.2.10"}, SiteProxyClientIPHeader: {"203.0.113.77"}}, "192.0.2.10"},
		{"未配置密钥时空密钥头不算相符", "", proxied("", "203.0.113.77"), "192.0.2.10"},
		{"未配置密钥", "", proxied(secret, "203.0.113.77"), "192.0.2.10"},
	} {
		if got := ClientAddress(request("10.0.0.1:1", tc.headers), "CF-Connecting-IP", tc.secret); got != tc.want {
			t.Errorf("%s: ClientAddress = %q, want %q", tc.name, got, tc.want)
		}
	}
	// 没有配置 client_ip_header 时，密钥相符的代理地址仍优先于 TCP 对端；密钥不符时只认 TCP 对端。
	if got := ClientAddress(request("10.0.0.1:1", proxied(secret, "203.0.113.78")), "", secret); got != "203.0.113.78" {
		t.Errorf("trusted proxy without client_ip_header = %q", got)
	}
	if got := ClientAddress(request("10.0.0.1:1", proxied("wrong", "203.0.113.78")), "", secret); got != "10.0.0.1" {
		t.Errorf("untrusted proxy without client_ip_header = %q", got)
	}
}

// 官网 BFF 带着正确的密钥转发时，账号接口按它报告的访客地址计额度；密钥不符的请求即使带了访客地址头，也和代理本身共用一份额度。
func TestAccountRateLimitsTrustSiteProxyOnlyWithSecret(t *testing.T) {
	db := testStore(t)
	secret := strings.Repeat("s", 40)
	a := &Service{store: db}
	a.ConfigureClientAddress("CF-Connecting-IP", secret)
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_rates WHERE key LIKE 'ip:%'`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, a)
	providers := func(proof, visitor string) {
		r := httptest.NewRequest("GET", "/v1/auth/providers", nil)
		r.RemoteAddr = "10.0.0.1:443"
		r.Header.Set("CF-Connecting-IP", "192.0.2.200")
		r.Header.Set(SiteProxyHeader, proof)
		r.Header.Set(SiteProxyClientIPHeader, visitor)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	providers(secret, "198.51.100.40")
	providers(secret, "198.51.100.41")
	providers("wrong", "198.51.100.42")
	providers("", "198.51.100.43")
	for address, want := range map[string]int{"198.51.100.40": 1, "198.51.100.41": 1, "198.51.100.42": 0, "198.51.100.43": 0, "192.0.2.200": 2} {
		var count int
		if err := db.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(count),0) FROM auth_rates WHERE key=$1`, "ip:"+hash(address)).Scan(&count); err != nil || count != want {
			t.Errorf("%s counted %d times, want %d (%v)", address, count, want, err)
		}
	}
}

// 在代理之后，账号接口和匿名开户按代理报告的地址计，代理后面的客户端不会共用一份额度。
func TestAccountRateLimitsUseConfiguredClientIPHeader(t *testing.T) {
	db := testStore(t)
	t.Setenv("ANON_HEADER_PEPPER", strings.Repeat("p", 32))
	a := &Service{store: db, clientIPHeader: "CF-Connecting-IP", config: Config{PepperEnv: "ANON_HEADER_PEPPER", Anonymous: AnonymousConfig{Enabled: true, DailyPerAddress: 1}}}
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_rates WHERE key LIKE 'anonymous:%' OR key LIKE 'ip:%'`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, a)
	challenge := func(client, subject string) int {
		r := httptest.NewRequest("POST", "/v1/auth/challenges", strings.NewReader(`{"provider":"anonymous","target":"`+subject+`"}`))
		r.RemoteAddr = "10.0.0.1:443"
		r.Header.Set("CF-Connecting-IP", client)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := challenge("198.51.100.30", "msime-header-000001"); code != 201 {
		t.Fatal(code)
	}
	if code := challenge("198.51.100.31", "msime-header-000002"); code != 201 {
		t.Fatal("a second client behind the same proxy was refused", code)
	}
	if code := challenge("198.51.100.30", "msime-header-000003"); code != 429 {
		t.Fatal("the daily per-address limit did not apply to the reported client", code)
	}
	var keys int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_rates WHERE key LIKE 'ip:%'`).Scan(&keys); err != nil || keys != 2 {
		t.Fatal("route bucket not keyed by client address", keys, err)
	}
}
