package server

import (
	"context"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 给一个用例开一套一次性 schema,并把 MSIME_TEST_DATABASE_URL 指过去。**不迁移** —— 要不要迁移由
// 用例自己决定,这正是自动迁移相关用例要验的东西。返回管理连接和 schema 名,方便直接查元数据。
func disposableSchema(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("MSIME_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("需要独立测试 PostgreSQL")
	}
	if !strings.Contains(dsn, "msime_auth_test") {
		t.Fatal("只能使用 msime_auth_test 测试数据库")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	// PostgreSQL 的标识符上限是 63 字节,所以截的是用例名而不是整个串 —— 截尾巴会把时间戳砍掉,
	// 反而制造重名。时间戳用纳秒:同一秒内跑完两个用例是常态,秒级精度会直接撞名。
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 40 {
		name = name[:40]
	}
	schema := name + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	t.Setenv("MSIME_TEST_DATABASE_URL", parsed.String())
	return admin, schema
}

// 空库直接起服务就该跑起来:迁移是纯增量、带 advisory lock 的,让运维记得先跑一次 -migrate-users
// 只是把一个可以自动做对的事变成一个会忘的事。
func TestStartupMigratesAnEmptyDatabase(t *testing.T) {
	admin, schema := disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("q", 48))
	config := func() Config {
		return Config{
			Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
			Admin:   AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN"},
			Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
		}
	}
	// 这里没有任何 Migrate 调用。
	s, err := New(config())
	if err != nil {
		t.Fatalf("an empty database did not migrate itself at startup: %v", err)
	}
	// 用户表、社区表、管理后台表（含站点设置）、译文缓存、候选框皮肤包和插件社区都归同一次迁移管,少一张都说明自动迁移漏了一段 schema。
	var tables int
	if err = admin.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.tables
 WHERE table_schema=$1 AND table_name IN
 ('auth_users','user_preferences','user_dictionary_entries','community_skins','admin_members','translation_cache','candidate_skins','candidate_skin_resources','community_candidate_skins','community_candidate_skin_files','community_candidate_skin_downloads','community_candidate_skin_ratings','community_plugins','community_plugin_downloads','community_plugin_ratings','site_settings')`,
		schema).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	s.CloseAccounts()
	s.Close()
	if tables != 16 {
		t.Fatalf("startup migration created %d of 16 expected tables", tables)
	}

	// 再起一次。迁移必须可重复执行 —— 多副本滚动升级时每个副本都会走这条路。
	again, err := New(config())
	if err != nil {
		t.Fatalf("starting against an already migrated database failed: %v", err)
	}
	again.CloseAccounts()
	again.Close()
}

// 用户表在、管理后台表不在:管理后台是后加的,这种库能通过 Ready 却卡在 AdminReady。
func TestStartupMigratesAdminTablesAddedLater(t *testing.T) {
	admin, schema := disposableSchema(t)
	ctx := context.Background()
	db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	quoted := pgx.Identifier{schema}.Sanitize()
	for _, table := range []string{"admin_members", "admin_events", "admin_audit", "admin_login_flows", "admin_sessions"} {
		if _, err = admin.Exec(ctx, "DROP TABLE "+quoted+"."+pgx.Identifier{table}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("q", 48))
	s, err := New(Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin:   AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN"},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
	})
	if err != nil {
		t.Fatalf("missing admin tables were not migrated at startup: %v", err)
	}
	s.CloseAccounts()
	s.Close()
}

// A database migrated before candidate skin packages existed passes the old readiness check; the new tables must still be created at startup.
func TestStartupMigratesCandidateSkinTablesAddedLater(t *testing.T) {
	admin, schema := disposableSchema(t)
	ctx := context.Background()
	db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "DROP TABLE "+quoted+".candidate_skin_resources, "+quoted+".candidate_skins"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	s, err := New(Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
	})
	if err != nil {
		t.Fatalf("missing candidate skin tables were not migrated at startup: %v", err)
	}
	defer s.Close()
	defer s.CloseAccounts()
	var tables int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name IN ('candidate_skins','candidate_skin_resources')`, schema).Scan(&tables); err != nil || tables != 2 {
		t.Fatal(tables, err)
	}
}

// A database migrated before community candidate skin packages existed keeps every older table, so only the Ready probe on each community_candidate_skin table makes startup create it. Each table is dropped on its own, so a missing probe for any one of them fails.
func TestStartupMigratesCommunityCandidateSkinTablesAddedLater(t *testing.T) {
	for _, table := range []string{"community_candidate_skins", "community_candidate_skin_files", "community_candidate_skin_downloads", "community_candidate_skin_ratings"} {
		t.Run(table, func(t *testing.T) {
			admin, schema := disposableSchema(t)
			ctx := context.Background()
			db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			db.Close()
			// CASCADE drops only the child tables' foreign keys when the parent table goes; the child tables stay.
			if _, err = admin.Exec(ctx, "DROP TABLE "+pgx.Identifier{schema}.Sanitize()+"."+pgx.Identifier{table}.Sanitize()+" CASCADE"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
			t.Setenv("TEST_CLIENT_TOKEN", testToken)
			s, err := New(Config{
				Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
				Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
			})
			if err != nil {
				t.Fatalf("missing %s was not migrated at startup: %v", table, err)
			}
			defer s.Close()
			defer s.CloseAccounts()
			var exists bool
			if err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2)`, schema, table).Scan(&exists); err != nil || !exists {
				t.Fatal(table, exists, err)
			}
		})
	}
}

// Every table, column and constraint the admin console added is probed at startup, so a database migrated before any one of them existed gets it created. Objects that public endpoints use are probed by Ready and must heal with the admin host disabled; the admin-only ones heal through AdminReady.
func TestStartupMigratesAdminConsoleObjectsAddedLater(t *testing.T) {
	public := []string{
		"community_reports", "word_submissions", "admin_crash_groups", "admin_notices", "admin_sensitive_words", "admin_sensitive_hits",
		"community_skins.moderation", "community_skins.moderated_by", "community_resources.previous_moderation", "community_candidate_skins.moderated_at", "community_candidate_skins.category", "community_skins.category", "community_plugins.moderation_reason",
		"auth_users.banned_at", "auth_users.ban_reason", "auth_users.banned_by", "auth_sessions.user_agent",
		"admin_events.artifact", "admin_events.channel", "admin_events.install_id", "admin_events.signature", "admin_events_kind_check", "community_plugins_kind_known",
	}
	adminOnly := []string{
		"admin_roles", "admin_role_permissions", "admin_tokens", "admin_notifications", "admin_notification_reads", "admin_preferences", "admin_service_metrics", "admin_service_daily", "admin_service_minutes", "admin_service_verdicts", "admin_incidents", "release_asset_snapshots",
		"admin_audit.detail", "admin_members.role", "admin_sessions.id", "admin_sessions.name", "admin_sessions.created_at", "admin_sessions.last_seen_at", "admin_sessions.user_agent",
	}
	for _, object := range append(append([]string{}, public...), adminOnly...) {
		t.Run(object, func(t *testing.T) {
			admin, schema := disposableSchema(t)
			ctx := context.Background()
			db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			db.Close()
			quoted := pgx.Identifier{schema}.Sanitize()
			table, column, isColumn := strings.Cut(object, ".")
			exists := `SELECT to_regclass($1||'.'||$2) IS NOT NULL`
			args := []any{schema, table}
			switch {
			case object == "admin_events_kind_check":
				_, err = admin.Exec(ctx, "ALTER TABLE "+quoted+".admin_events DROP CONSTRAINT admin_events_kind_check, ADD CONSTRAINT admin_events_kind_check CHECK(kind IN ('download','crash'))")
				exists = `SELECT EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname=$1 AND c.conname=$2 AND pg_get_constraintdef(c.oid) LIKE '%session_crash%')`
			case object == "community_plugins_kind_known":
				// 新增辅助码表、符号集、短语表和单词本之前的约束。
				_, err = admin.Exec(ctx, "ALTER TABLE "+quoted+".community_plugins DROP CONSTRAINT community_plugins_kind_known, ADD CONSTRAINT community_plugins_kind_known CHECK(kind IN ('sound','music','command_table','effect'))")
				exists = `SELECT EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname=$1 AND c.conname=$2 AND pg_get_constraintdef(c.oid) LIKE '%wordbook%')`
			case isColumn:
				_, err = admin.Exec(ctx, "ALTER TABLE "+quoted+"."+pgx.Identifier{table}.Sanitize()+" DROP COLUMN "+pgx.Identifier{column}.Sanitize()+" CASCADE")
				exists = `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3)`
				args = append(args, column)
			default:
				_, err = admin.Exec(ctx, "DROP TABLE "+quoted+"."+pgx.Identifier{table}.Sanitize()+" CASCADE")
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
			t.Setenv("TEST_CLIENT_TOKEN", testToken)
			t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("q", 48))
			config := Config{
				Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
				Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
			}
			if slices.Contains(adminOnly, object) {
				config.Admin = AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN"}
			}
			s, err := New(config)
			if err != nil {
				t.Fatalf("missing %s was not migrated at startup: %v", object, err)
			}
			s.CloseAccounts()
			s.Close()
			var restored bool
			if err = admin.QueryRow(ctx, exists, args...).Scan(&restored); err != nil || !restored {
				t.Fatal(object, "not restored", err)
			}
		})
	}
}

// 旧库的 user_dictionary_entries.kind 约束只有四种取值。启动时 Ready 发现约束不含 wubi98 就走迁移：约束被原名替换为含 wubi98 的版本，已有词条一行不动，98 版五笔词条可以写入，未知种类照样被拒绝。
func TestStartupRelaxesDictionaryKindConstraintForWubi98(t *testing.T) {
	admin, schema := disposableSchema(t)
	ctx := context.Background()
	db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	quoted := pgx.Identifier{schema}.Sanitize()
	// 还原成加入 wubi98 之前的列约束（PostgreSQL 给列约束起的默认名就是这个），并放进一条已有词条。
	if _, err = admin.Exec(ctx, `ALTER TABLE `+quoted+`.user_dictionary_entries DROP CONSTRAINT user_dictionary_entries_kind_check, ADD CONSTRAINT user_dictionary_entries_kind_check CHECK(kind IN ('pinyin','wubi','english','quick'));
INSERT INTO `+quoted+`.auth_users(id,display_name) VALUES('wubi98-migration-user','');
INSERT INTO `+quoted+`.user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES('existing-entry','wubi98-migration-user','wubi','wq','你',10,1)`); err != nil {
		t.Fatal(err)
	}
	insert := func(id, kind string) error {
		_, err := admin.Exec(ctx, `INSERT INTO `+quoted+`.user_dictionary_entries(id,user_id,kind,code,word,weight,revision) VALUES($1,'wubi98-migration-user',$2,'kg',$1,10,2)`, id, kind)
		return err
	}
	if insert("before-migration", "wubi98") == nil {
		t.Fatal("the old constraint accepted wubi98")
	}
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	config := Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
	}
	constraints := func() []string {
		t.Helper()
		rows, err := admin.Query(ctx, `SELECT c.conname||' '||pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname=$1 AND c.conrelid=($2||'.user_dictionary_entries')::regclass AND c.contype='c' AND pg_get_constraintdef(c.oid) LIKE '%kind%' ORDER BY 1`, schema, quoted)
		if err != nil {
			t.Fatal(err)
		}
		defs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return defs
	}
	// 迁移可以重复执行：第二次启动时约束已是新的，不再替换，也不会多出一条。
	for range 2 {
		s, err := New(config)
		if err != nil {
			t.Fatalf("the old dictionary kind constraint was not migrated at startup: %v", err)
		}
		s.CloseAccounts()
		s.Close()
		defs := constraints()
		if len(defs) != 1 || !strings.HasPrefix(defs[0], "user_dictionary_entries_kind_check ") || !strings.Contains(defs[0], "'wubi98'") {
			t.Fatal("kind constraint after migration:", defs)
		}
	}
	var kind string
	if err = admin.QueryRow(ctx, `SELECT kind FROM `+quoted+`.user_dictionary_entries WHERE id='existing-entry'`).Scan(&kind); err != nil || kind != "wubi" {
		t.Fatal("existing entry changed", kind, err)
	}
	if err = insert("after-migration", "wubi98"); err != nil {
		t.Fatal("wubi98 rejected after migration:", err)
	}
	if insert("unknown-kind", "wubi86") == nil {
		t.Fatal("the new constraint accepted an unknown kind")
	}
}

// notificationRows lists the console notifications recorded in schema as "kind page id", oldest first, so a test can check that a GitHub-driven write really reached the bell.
func notificationRows(t *testing.T, conn *pgx.Conn, schema string) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT kind||' '||target_page||' '||target_id FROM `+pgx.Identifier{schema, "admin_notifications"}.Sanitize()+` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Rerunning the migration must not add constraints. PostgreSQL 12 re-adds the inline CHECK of ADD COLUMN IF NOT EXISTS on every run, so those checks are added by name; the copies an earlier rerun left on PostgreSQL 12 (name1, name2, ...) are dropped.
func TestMigrationRerunKeepsConstraints(t *testing.T) {
	admin, schema := disposableSchema(t)
	ctx := context.Background()
	db, err := account.Open(ctx, os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname=$1`, schema).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	first := count()
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, `ALTER TABLE `+quoted+`.admin_events ADD CONSTRAINT admin_events_artifact_check1 CHECK(length(artifact) BETWEEN 1 AND 64);
ALTER TABLE `+quoted+`.community_skins ADD CONSTRAINT community_skins_moderation_check1 CHECK(moderation IN ('pending','approved','removed'))`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != first {
			t.Fatalf("migration rerun changed the constraint count from %d to %d", first, n)
		}
	}
	for _, name := range []string{"admin_events_artifact_check", "community_skins_moderation_check", "admin_sessions_user_agent_check", "auth_users_ban_reason_check", "community_candidate_skins_visibility_check"} {
		var exists bool
		if err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname=$1 AND c.conname=$2)`, schema, name).Scan(&exists); err != nil || !exists {
			t.Fatal(name, exists, err)
		}
	}
}
