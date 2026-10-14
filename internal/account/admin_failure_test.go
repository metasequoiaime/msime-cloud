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

// 每个数据库失败用例都重新建立这些状态，保证逐条语句取消时使用相同的数据和有效的来源会话。
type consoleFailureFixture struct {
	user, token, adminSession string
}

func seedConsoleFailureFixture(t *testing.T, db *Store) consoleFailureFixture {
	t.Helper()
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_members,admin_sessions,admin_audit,admin_tokens,admin_preferences,admin_notifications,admin_notification_reads,admin_notices,admin_crash_groups,admin_events,admin_incidents,admin_sensitive_words,admin_sensitive_hits,admin_service_daily,community_reports,site_settings,auth_users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	// The permissions grant case changes the shared role matrix, which TRUNCATE does not reset.
	if _, err := db.pool.Exec(ctx, `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`); err != nil {
		t.Fatal(err)
	}
	user := complete(t, db, Identity{"email", randomToken() + "@example.test"})
	uid := user.User.ID
	for _, q := range []string{
		`INSERT INTO community_skins(id,owner_id,name,description,design,moderation) VALUES('fail-skin',$1,'春日樱','粉色','{}','pending')`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('fail-dict',$1,'dictionary','前端术语','{"entries":[{"kind":"pinyin","code":"fangdou","word":"防抖","weight":10}]}','pending'),('fail-reply',$1,'reply','委婉拒绝','{"prompt":"今天没法加班"}','removed')`,
		`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES('fa334455-1234-4234-8234-000000000001',$1,'sound','fail.sound','Plugin','1.0','MIT','id = "fail.sound"','zip','` + hash("fail-plugin") + `')`,
		`INSERT INTO community_reports(kind,item_id,reporter_id,reason) VALUES('skins','fail-skin',$1,'广告')`,
		`INSERT INTO admin_events(id,kind,platform,version,install_id,message,signature,created_at) VALUES
 ('fail-active','active','windows','1','device-windows-0001','',NULL,now()),
 ('fail-session','session','ios','1',NULL,'',NULL,now()),
 ('fail-session-crash','session_crash','ios','1',NULL,'',NULL,now()),
 ('fail-download','download','windows','1',NULL,'',NULL,now()),
 ('fail-crash','crash','windows','1','device-windows-0001','boom','0123456789abcdef',now())`,
		`INSERT INTO admin_crash_groups(signature,platform,version,title) VALUES('0123456789abcdef','windows','1','boom')`,
		`INSERT INTO admin_notices(title,body,targets,channels,created_by) VALUES('草稿','正文','{all}','{site}','google:o:owner@example.test')`,
		`INSERT INTO admin_incidents(service,title) VALUES('chat','联想超时')`,
		`INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('加v','ad','review','google:o:owner@example.test')`,
		`INSERT INTO admin_notifications(kind,title,target_page,target_id) VALUES('report','举报','community','fail-skin')`,
		`INSERT INTO admin_members(email,role) VALUES('member@example.test','reviewer')`,
		`INSERT INTO admin_sessions(token_hash,subject,email,name,created_at) VALUES('fail-session-hash','o','owner@example.test','Owner',now())`,
		`INSERT INTO admin_service_daily(service,day,ok_minutes,total_minutes,degraded,p95_ms) VALUES('chat',(now() AT TIME ZONE 'UTC')::date,50,60,true,900)`,
	} {
		var err error
		if strings.Contains(q, "$1") {
			_, err = db.pool.Exec(ctx, q, uid)
		} else {
			_, err = db.pool.Exec(ctx, q)
		}
		if err != nil {
			t.Fatal(q, err)
		}
	}
	insertCandidateSkin(t, db, "fa334455-1234-4234-8234-123456789abc", uid, "Candidate")
	session, err := (&Service{store: db}).CreateAdminSession(ctx, AdminIdentity{Subject: "o", Email: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	return consoleFailureFixture{user: uid, token: user.AccessToken, adminSession: session}
}

// Every console endpoint answers a failed database statement with 503, or, where a statement only feeds optional context (hit counts written back, notes), with the answer it gives without it. Each statement of each request is cancelled in turn at the driver, against freshly seeded rows.
func TestConsoleEndpointsAnswerEveryDatabaseFailure(t *testing.T) {
	db := testStore(t)
	t.Cleanup(func() {
		db.pool.Exec(context.Background(), `DELETE FROM admin_role_permissions WHERE role='readonly' AND permission='ban_users'`)
	})
	owner := AdminAccess{Actor: "google:o:owner@example.test", Email: "owner@example.test", Role: RoleMaintainer, Permissions: AllAdminPermissions(), Owner: true}
	cases := []struct {
		name, method, path string
		body               func(consoleFailureFixture) string
	}{
		{"overview", "GET", "/api/overview?days=7", nil},
		{"notifications", "GET", "/api/notifications", nil},
		{"notifications read", "POST", "/api/notifications/read", func(consoleFailureFixture) string { return `{"ids":[1]}` }},
		{"notifications read all", "POST", "/api/notifications/read", func(consoleFailureFixture) string { return `{"all":true}` }},
		{"notifications read all up to", "POST", "/api/notifications/read", func(consoleFailureFixture) string { return `{"all":true,"up_to_id":"1"}` }},
		{"me", "GET", "/api/me", nil},
		{"me set pref", "POST", "/api/me", func(consoleFailureFixture) string { return `{"action":"set_pref","key":"notify_report","value":false}` }},
		{"me regenerate token", "POST", "/api/me", func(consoleFailureFixture) string { return `{"action":"regenerate_token"}` }},
		{"permissions", "GET", "/api/permissions", nil},
		{"permissions grant", "POST", "/api/permissions", func(consoleFailureFixture) string {
			return `{"action":"grant","role":"readonly","permission":"ban_users"}`
		}},
		{"user stats", "GET", "/api/users/stats", nil},
		{"user", "GET", "/api/users/{user}", nil},
		{"community counts", "GET", "/api/community/counts", nil},
		{"skin", "GET", "/api/skins/fail-skin", nil},
		{"candidate skin", "GET", "/api/candidate-skins/fa334455-1234-4234-8234-123456789abc", nil},
		{"candidate skin preview", "GET", "/api/candidate-skins/fa334455-1234-4234-8234-123456789abc/preview", nil},
		{"plugin", "GET", "/api/plugins/fa334455-1234-4234-8234-000000000001", nil},
		{"dictionary", "GET", "/api/dictionaries/fail-dict", nil},
		{"sensitive words", "GET", "/api/sensitive-words", nil},
		{"downloads", "GET", "/api/downloads/summary", nil},
		{"notices", "GET", "/api/notices", nil},
		{"crash groups", "GET", "/api/crash-groups", nil},
		{"crash group", "GET", "/api/crash-groups/0123456789abcdef", nil},
		{"ban", "POST", "/api/actions", func(f consoleFailureFixture) string {
			return `{"action":"ban_user","id":"` + f.user + `","reason":"广告"}`
		}},
		{"approve", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"approve_content","section":"skins","id":"fail-skin"}`
		}},
		{"remove", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"remove_content","section":"dictionaries","id":"fail-dict","reason":"广告"}`
		}},
		{"restore", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"restore_content","section":"replies","id":"fail-reply"}`
		}},
		{"skin category", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"set_skin_category","id":"fail-skin","value":{"category":"food"}}`
		}},
		{"candidate category", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"set_candidate_skin_category","id":"fa334455-1234-4234-8234-123456789abc","value":{"category":"food"}}`
		}},
		{"restore to", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"restore_content","section":"skins","id":"fail-skin","value":{"to":"approved"}}`
		}},
		{"add sensitive word", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"add_sensitive_word","value":{"pattern":"/微\\s*信/","category":"ad","level":"block"}}`
		}},
		{"sensitive level", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"set_sensitive_word_level","ids":["1"],"value":"block"}`
		}},
		{"delete sensitive word", "POST", "/api/actions", func(consoleFailureFixture) string { return `{"action":"delete_sensitive_word","ids":["1"]}` }},
		{"save notice", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"save_notice_draft","id":"1","value":{"title":"标题","body":"正文","targets":["all"],"channels":["site"]}}`
		}},
		{"publish notice", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"publish_notice","id":"1","value":{"title":"标题","body":"正文","targets":["all"],"channels":["site"]}}`
		}},
		{"crash status", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"crash_group_status","id":"0123456789abcdef","value":"known"}`
		}},
		{"update incident", "POST", "/api/actions", func(consoleFailureFixture) string {
			return `{"action":"update_incident","id":"1","value":{"description":"已切换备用通道"},"reason":"补充"}`
		}},
		{"resolve incident", "POST", "/api/actions", func(consoleFailureFixture) string { return `{"action":"resolve_incident","id":"1"}` }},
		{"site settings", "POST", "/api/site-settings", func(consoleFailureFixture) string { return `{"lanzou_url":"https://example.com/a"}` }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			a := &Service{store: &Store{pool: pool}}
			a.ConfigureAdmin(AdminSettings{Services: []AdminService{{Key: "chat", Name: "AI 联想"}}})
			call := func(f consoleFailureFixture) *httptest.ResponseRecorder {
				body := ""
				if tc.body != nil {
					body = tc.body(f)
				}
				r := jsonRequest(tc.method, strings.ReplaceAll(tc.path, "{user}", f.user), body, "")
				r = r.WithContext(WithAdminSession(r.Context(), f.adminSession))
				r = r.WithContext(WithAdminAccess(r.Context(), owner))
				w := httptest.NewRecorder()
				a.AdminHTTP(w, r)
				return w
			}
			baseline := call(seedConsoleFailureFixture(t, db))
			if baseline.Code >= 300 {
				t.Fatal("baseline", baseline.Code, baseline.Body.String())
			}
			trace.mu.Lock()
			statements := len(trace.statements)
			trace.mu.Unlock()
			if statements == 0 {
				t.Fatal("no database statement traced")
			}
			for i := 1; i <= statements; i++ {
				f := seedConsoleFailureFixture(t, db)
				trace.mu.Lock()
				trace.at, trace.seen, trace.statements = i, 0, nil
				trace.mu.Unlock()
				w := call(f)
				trace.mu.Lock()
				sql := ""
				if len(trace.statements) >= i {
					sql = trace.statements[i-1]
				}
				trace.at = 0
				trace.mu.Unlock()
				if w.Code != 503 && w.Code != baseline.Code {
					t.Fatalf("statement %d (%s): HTTP %d %s", i, sql, w.Code, w.Body.String())
				}
				if !json.Valid(w.Body.Bytes()) && w.Header().Get("Content-Type") != baseline.Header().Get("Content-Type") {
					t.Fatalf("statement %d: unexpected body %q", i, w.Body.String())
				}
			}
		})
	}
}

// The client and website endpoints the console added or changed answer a failed database statement the same way.
func TestConsolePublicEndpointsAnswerEveryDatabaseFailure(t *testing.T) {
	db := testStore(t)
	cases := []struct {
		name, method, path string
		body               string
		user               bool
	}{
		{"publish skin", "POST", "/v1/community/skins", `{"id":"fb334455-1234-4234-8234-000000000001","name":"秋色","description":"示例","design":` + communityFixture + `}`, true},
		{"publish reply", "POST", "/v1/community/resources", `{"id":"fb334455-1234-4234-8234-000000000002","kind":"reply","name":"模板","description":"","content":{"prompt":"礼貌回复"}}`, true},
		{"report", "POST", "/v1/community/reports", `{"kind":"skins","item_id":"fail-skin","reason":"广告"}`, true},
		{"notices", "GET", "/v1/notices?platform=windows&channel=site", "", false},
		{"crash telemetry", "POST", "/v1/telemetry/events", `{"id":"telemetry-failure-crash-01","kind":"crash","platform":"windows","version":"1","message":"boom","stack":"at main.go:1\nat runtime.go:2","install_id":"device-windows-0001"}`, false},
		{"active telemetry", "POST", "/v1/telemetry/events", `{"id":"telemetry-failure-active-1","kind":"active","platform":"windows","version":"1","install_id":"device-windows-0002"}`, false},
		{"site mirrors", "GET", "/v1/site/download-mirrors", "", false},
		{"skins", "GET", "/v1/community/skins", "", true},
		{"skin", "GET", "/v1/community/skins/fail-skin", "", true},
		{"skin category", "PATCH", "/v1/community/skins/fail-skin", `{"category":"food"}`, true},
		{"delete skin", "DELETE", "/v1/community/skins/fail-skin", "", true},
		{"resources", "GET", "/v1/community/resources?kind=dictionary", "", true},
		{"resource", "GET", "/v1/community/resources/fail-dict", "", true},
		{"delete resource", "DELETE", "/v1/community/resources/fail-dict", "", true},
		{"candidate skins", "GET", "/v1/community/candidate-skins", "", true},
		{"candidate skin", "GET", "/v1/community/candidate-skins/fa334455-1234-4234-8234-123456789abc", "", true},
		{"candidate preview", "GET", "/v1/community/candidate-skins/fa334455-1234-4234-8234-123456789abc/preview", "", true},
		{"candidate visibility", "PATCH", "/v1/community/candidate-skins/fa334455-1234-4234-8234-123456789abc", `{"visibility":"private"}`, true},
		{"candidate category", "PATCH", "/v1/community/candidate-skins/fa334455-1234-4234-8234-123456789abc", `{"category":"food"}`, true},
		{"delete candidate", "DELETE", "/v1/community/candidate-skins/fa334455-1234-4234-8234-123456789abc", "", true},
		{"plugins", "GET", "/v1/community/plugins", "", true},
		{"plugin", "GET", "/v1/community/plugins/fa334455-1234-4234-8234-000000000001", "", true},
		{"delete plugin", "DELETE", "/v1/community/plugins/fa334455-1234-4234-8234-000000000001", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			a := &Service{store: &Store{pool: pool}}
			mux := http.NewServeMux()
			Mount(mux, a)
			// The server package mounts these two next to the account routes.
			mux.HandleFunc("POST /v1/community/reports", Route(a, "POST /v1/community/reports", (*Service).CommunityReport))
			mux.HandleFunc("GET /v1/notices", Route(a, "GET /v1/notices", (*Service).PublicNotices))
			call := func(f consoleFailureFixture) *httptest.ResponseRecorder {
				token := ""
				if tc.user {
					token = f.token
				}
				r := jsonRequest(tc.method, tc.path, tc.body, token)
				w := httptest.NewRecorder()
				if strings.HasPrefix(tc.path, "/v1/telemetry/") {
					a.Telemetry(w, r)
				} else {
					mux.ServeHTTP(w, r)
				}
				return w
			}
			trace.mu.Lock()
			trace.statements = nil
			trace.mu.Unlock()
			baseline := call(seedConsoleFailureFixture(t, db))
			if baseline.Code >= 300 {
				t.Fatal("baseline", baseline.Code, baseline.Body.String())
			}
			trace.mu.Lock()
			statements := len(trace.statements)
			trace.mu.Unlock()
			if statements == 0 {
				t.Fatal("no database statement traced")
			}
			for i := 1; i <= statements; i++ {
				f := seedConsoleFailureFixture(t, db)
				trace.mu.Lock()
				trace.at, trace.seen, trace.statements = i, 0, nil
				trace.mu.Unlock()
				w := call(f)
				trace.mu.Lock()
				trace.at = 0
				trace.mu.Unlock()
				if w.Code < 500 && w.Code != baseline.Code {
					t.Fatalf("statement %d: HTTP %d %s", i, w.Code, w.Body.String())
				}
			}
		})
	}
}
