package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func TestServiceLifecycleAndUnavailableDatabase(t *testing.T) {
	db := testStore(t)
	t.Setenv("SERVICE_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("SERVICE_TEST_PEPPER", strings.Repeat("p", 32))
	cfg := Config{Enabled: true, DatabaseEnv: "SERVICE_TEST_DB", PepperEnv: "SERVICE_TEST_PEPPER"}
	a, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.ConfigureEngine(engine.Config{})
	a.Close()
	a.Close()
	var disabled *Service
	disabled.Close()
	disabled.ConfigureEngine(engine.Config{})
	if _, err := disabled.Authenticate(t.Context(), "x"); err != ErrInvalid {
		t.Fatal(err)
	}
	if disabled, err = New(t.Context(), Config{}); err != nil || disabled != nil {
		t.Fatal(disabled, err)
	}
	if _, err = New(t.Context(), Config{Enabled: true}); err == nil {
		t.Fatal("missing config accepted")
	}
	t.Setenv("SERVICE_TEST_DB", "not-a-postgres-connection-string")
	if _, err = New(t.Context(), cfg); err == nil {
		t.Fatal("invalid DSN accepted")
	}
	if _, err := db.pool.Exec(t.Context(), `DROP TABLE user_preferences CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVICE_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	// 配了迁移角色就必须切过去建表。生产上运行角色只有 DML 权限,DDL 只有属主角色做得了;要是这里
	// 悄悄用连接自己的身份,线上表现就是 v0.21.0 那次:版本新增一张表 → 缺表 → DDL 被拒 → 启动失败
	// → 整个后端 CrashLoopBackOff。用一个不存在的角色钉住这条路由:切换确实发生了。
	cfg.MigrationRole = "msime_migration_role_that_does_not_exist"
	if _, err = New(t.Context(), cfg); err == nil {
		t.Fatal("migration ignored the configured role and used the connection identity")
	}
	// 留空则用连接自己的身份,单角色的开发和测试部署照旧自愈。
	cfg.MigrationRole = ""
	// 缺表不再是启动失败,而是就地补迁移 —— 版本升级新增一张表时不该要求运维记得先跑 -migrate-users。
	// 连不上数据库仍然失败,由上面那条无效 DSN 用例覆盖。
	migrated, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal("missing table was not migrated at startup:", err)
	}
	migrated.Close()
	// to_regclass 按这条连接自己的 search_path 解析,不会撞上其它用例留在同一个库里的一次性 schema。
	var restored bool
	if err = db.pool.QueryRow(t.Context(), `SELECT to_regclass('user_preferences') IS NOT NULL`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("startup reported success without recreating the missing table")
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	a = &Service{store: db}
	for _, path := range []string{"overview", "users", "downloads", "crashes", "skins", "dictionaries", "replies", "audit", "users/missing", "skins/missing", "dictionaries/missing", "replies/missing"} {
		w := httptest.NewRecorder()
		a.AdminHTTP(w, httptest.NewRequest("GET", "/api/"+path, nil))
		if w.Code != 503 || strings.Contains(w.Body.String(), "closed pool") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		a.AdminMembersHTTP(w, jsonRequest(method, "/api/admins", `{"email":"member@example.test","action":"add"}`, ""), nil)
		if w.Code != 503 {
			t.Fatal(w.Code)
		}
	}
	w := httptest.NewRecorder()
	a.adminAction(w, adminJSONRequest("POST", "/api/actions", `{"id":"user","action":"revoke_sessions"}`))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	a.Telemetry(w, jsonRequest("POST", "/v1/telemetry/events", `{"id":"failure-event-0001","kind":"download","platform":"ios","version":"1"}`, ""))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	disabled.Telemetry(w, httptest.NewRequest("POST", "/v1/telemetry/events", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func jsonRequest(method, path, body, token string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestReadOnlyDatabaseRejectsMutationsWithoutSuccess(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "readonly@example.test"})
	config := db.pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a := &Service{store: &Store{pool: pool}}
	for _, tc := range []struct {
		name, method, path, body string
		handler                  func(http.ResponseWriter, *http.Request)
	}{
		{"profile", "PATCH", "/v1/users/me", `{"display_name":"not committed"}`, a.update},
		{"logout", "POST", "/v1/auth/logout", `{}`, a.logout},
		{"delete", "DELETE", "/v1/users/me", "", a.delete},
		{"clipboard setting", "PUT", "/v1/users/me/clipboard/settings", `{"enabled":true}`, a.clipboardSettings},
		{"preferences", "PUT", "/v1/users/me/preferences", `{"revision":0,"settings":{}}`, a.preferences},
		{"admin action", "POST", "/api/actions", `{"id":"` + user.User.ID + `","action":"revoke_sessions"}`, func(w http.ResponseWriter, r *http.Request) {
			a.adminAction(w, r.WithContext(adminTestContext(r.Context(), "legacy-token")))
		}},
		{"telemetry", "POST", "/v1/telemetry/events", `{"id":"readonly-event-0001","kind":"download","platform":"ios","version":"1"}`, a.Telemetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.handler(w, jsonRequest(tc.method, tc.path, tc.body, user.AccessToken))
			if w.Code != 503 || strings.Contains(w.Body.String(), "read-only") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	if _, err := db.Authenticate(context.Background(), user.AccessToken); err != nil {
		t.Fatal("failed logout revoked session", err)
	}
	profile, _, err := db.Me(t.Context(), user.User.ID)
	if err != nil || profile.DisplayName == "not committed" {
		t.Fatal("failed update committed", err)
	}
}

// Android 改版新增的账号接口（会话、短语同步、反馈、云端数据、剪贴板保留与置顶、诊断快照、候选窗皮肤同步与替换）在任意一条数据库语句失败时返回 503，只是可选语句失败时照常应答；写操作失败后调用者的数据与请求前完全一致。每个请求的每条语句依次在驱动层取消，每次都重新准备数据。
func TestAndroidAccountEndpointsAnswerEveryDatabaseFailure(t *testing.T) {
	db := testStore(t)
	type fixture struct {
		user, token, otherSession, clipboard, candidate string
	}
	seed := func(t *testing.T) fixture {
		t.Helper()
		identity := Identity{"email", randomToken() + "@example.test"}
		first := complete(t, db, identity)
		second := complete(t, db, identity)
		uid := first.User.ID
		var other string
		if err := db.pool.QueryRow(t.Context(), `SELECT id FROM auth_sessions WHERE user_id=$1 AND refresh_hash=$2`, uid, hash(second.RefreshToken)).Scan(&other); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PutPhrases(t.Context(), uid, 0, []Phrase{{ID: "p1", Text: "我在开会，稍后回复", Group: "工作", Position: 0}}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetClipboardEnabled(t.Context(), uid, true); err != nil {
			t.Fatal(err)
		}
		item, err := db.AddClipboard(t.Context(), uid, "剪贴板条目")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO diagnostic_snapshots(id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,expires_at) VALUES($1,$2,$3,'abcd','android','1.0.0','{perf_trace}','{"perf_trace":[]}',17,now()+interval '1 hour')`, randomToken()[:24], uid, hash(randomToken())); err != nil {
			t.Fatal(err)
		}
		candidate := randomToken()[:8] + "-1234-4234-8234-" + randomToken()[:12]
		insertCandidateSkin(t, db, candidate, uid, "候选窗皮肤")
		return fixture{uid, first.AccessToken, other, item.ID, candidate}
	}
	// state 是下面这些请求可能为该用户写入的全部数据，经不带追踪的连接池读取。
	state := func(t *testing.T, uid string) string {
		t.Helper()
		var all strings.Builder
		for _, q := range []string{
			`SELECT to_jsonb(x) FROM user_phrases x WHERE user_id=$1`,
			`SELECT to_jsonb(x) FROM user_preferences x WHERE user_id=$1`,
			`SELECT to_jsonb(x) FROM user_clipboard x WHERE user_id=$1`,
			`SELECT to_jsonb(x) FROM user_clipboard_settings x WHERE user_id=$1`,
			`SELECT to_jsonb(x) FROM feedback x WHERE user_id=$1`,
			`SELECT jsonb_build_object('feedback_id',feedback_id,'position',position) FROM feedback_screenshots WHERE feedback_id IN (SELECT id FROM feedback WHERE user_id=$1)`,
			`SELECT jsonb_build_object('id',id,'token_hash',token_hash,'expires_at',expires_at) FROM diagnostic_snapshots WHERE user_id=$1`,
			`SELECT jsonb_build_object('id',id,'revoked',revoked) FROM auth_sessions WHERE user_id=$1`,
			`SELECT jsonb_build_object('id',id,'name',name,'request_sha256',request_sha256,'updated_at',updated_at) FROM community_candidate_skins WHERE owner_id=$1`,
			`SELECT jsonb_build_object('skin_id',skin_id,'path',path,'bytes',md5(bytes)) FROM community_candidate_skin_files WHERE skin_id IN (SELECT id FROM community_candidate_skins WHERE owner_id=$1)`,
		} {
			var raw string
			if err := db.pool.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (`+q+`) AS s(row)`, uid).Scan(&raw); err != nil {
				t.Fatal(q, err)
			}
			all.WriteString(raw)
		}
		return all.String()
	}
	png := feedbackPNG(t, 4, 3)
	manifest, files := candidateFixture(t, "shared")
	replacement, err := json.Marshal(map[string]any{"name": "换了新名字", "description": "A shared skin", "manifest": manifest, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, path string
		// body 返回请求体及其 Content-Type，为 nil 表示没有请求体。
		body func(fixture) (string, string)
		// write 标记失败后不能留下部分修改的请求。
		write bool
	}{
		{"sessions", "GET", "/v1/users/me/sessions", nil, false},
		{"revoke session", "DELETE", "/v1/users/me/sessions/{other}", nil, true},
		{"phrases", "GET", "/v1/users/me/phrases", nil, false},
		{"put phrases", "PUT", "/v1/users/me/phrases", func(fixture) (string, string) {
			return `{"revision":1,"phrases":[{"id":"p2","text":"马上到","group":"","position":0}]}`, "application/json"
		}, true},
		{"feedback", "POST", FeedbackPath, func(f fixture) (string, string) {
			body, contentType := feedbackForm(t, payloadPart(validFeedbackPayload), screenshotPart(png, "image/png"))
			return body.String(), contentType
		}, true},
		{"data summary", "GET", "/v1/users/me/data", nil, false},
		{"data export", "GET", "/v1/users/me/data/export", nil, false},
		{"data delete", "DELETE", "/v1/users/me/data", func(fixture) (string, string) {
			return `{"sections":["phrases","clipboard","preferences","voice"]}`, "application/json"
		}, true},
		{"clipboard retention", "PUT", "/v1/users/me/clipboard/retention", func(fixture) (string, string) { return `{"days":7}`, "application/json" }, true},
		{"clipboard pin", "PUT", "/v1/users/me/clipboard/{clipboard}/pin", func(fixture) (string, string) { return `{"pinned":true}`, "application/json" }, true},
		{"diagnostics upload", "POST", "/v1/users/me/diagnostics", func(fixture) (string, string) {
			return diagnosticBody(`{"perf_trace":[{"t_ms":1,"kind":"key_down","duration_ms":3}]}`), "application/json"
		}, true},
		{"diagnostics read", "GET", "/v1/users/me/diagnostics", nil, false},
		{"diagnostics token", "POST", "/v1/users/me/diagnostics/token", nil, true},
		{"diagnostics delete", "DELETE", "/v1/users/me/diagnostics", nil, true},
		{"candidate sync", "GET", "/v1/community/candidate-skins/sync", nil, false},
		{"candidate replace", "PUT", "/v1/community/candidate-skins/{candidate}", func(fixture) (string, string) { return string(replacement), "application/json" }, true},
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
			mux := http.NewServeMux()
			Mount(mux, &Service{store: &Store{pool: pool}})
			call := func(f fixture) (w *httptest.ResponseRecorder, aborted bool) {
				body, contentType := "", ""
				if tc.body != nil {
					body, contentType = tc.body(f)
				}
				path := strings.NewReplacer("{other}", f.otherSession, "{clipboard}", f.clipboard, "{candidate}", f.candidate).Replace(tc.path)
				r := httptest.NewRequest(tc.method, path, strings.NewReader(body))
				if contentType != "" {
					r.Header.Set("Content-Type", contentType)
				}
				r.Header.Set("Authorization", "Bearer "+f.token)
				// 每次请求用一个不同的文档保留地址，避免按地址的共享限流代替接口本身作答。
				r.RemoteAddr = "[2001:db8::" + randomToken()[:4] + "]:443"
				w = httptest.NewRecorder()
				// 导出在写出响应头后才流式写 zip，此后出错只能中断连接，这正是约定的行为。
				defer func() {
					if v := recover(); v != nil {
						if v != http.ErrAbortHandler {
							panic(v)
						}
						aborted = true
					}
				}()
				mux.ServeHTTP(w, r)
				return w, false
			}
			baseline, aborted := call(seed(t))
			if aborted || baseline.Code >= 300 {
				t.Fatal("baseline", aborted, baseline.Code, baseline.Body.String())
			}
			trace.mu.Lock()
			statements := len(trace.statements)
			trace.mu.Unlock()
			if statements == 0 {
				t.Fatal("no database statement traced")
			}
			for i := 1; i <= statements; i++ {
				f := seed(t)
				before := state(t, f.user)
				trace.mu.Lock()
				trace.at, trace.seen, trace.statements = i, 0, nil
				trace.mu.Unlock()
				w, aborted := call(f)
				trace.mu.Lock()
				sql := ""
				if len(trace.statements) >= i {
					sql = trace.statements[i-1]
				}
				trace.at = 0
				trace.mu.Unlock()
				switch {
				case aborted:
					if tc.name != "data export" {
						t.Fatalf("statement %d (%s): handler aborted", i, sql)
					}
				case w.Code == 503:
					if !strings.Contains(w.Body.String(), `"auth_unavailable"`) {
						t.Fatalf("statement %d (%s): %s", i, sql, w.Body.String())
					}
					if tc.write {
						if after := state(t, f.user); after != before {
							t.Fatalf("statement %d (%s): partial write persisted", i, sql)
						}
					}
				case w.Code != baseline.Code:
					t.Fatalf("statement %d (%s): HTTP %d %s", i, sql, w.Code, w.Body.String())
				}
			}
		})
	}
}
