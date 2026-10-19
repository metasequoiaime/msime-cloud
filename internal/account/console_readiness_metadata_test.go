package account

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type readinessMetadataFixture struct {
	table, name, missing string
	constraint           bool
}

func TestReadyPreservesMissingMetadataOrder(t *testing.T) {
	checkMissingReadinessMetadataOrder(t, (*Store).Ready, []readinessMetadataFixture{
		{"candidate_skins", "candidate_skins_id_check", "candidate skin id constraints predate the msime-windows looks", true},
		{"candidate_skin_resources", "candidate_skin_resources_path_check", "candidate skin resource constraint predates APNG paths", true},
		{"community_candidate_skin_files", "community_candidate_skin_files_path_check", "community candidate resource constraint predates canvas resources", true},
		{"community_resources", "community_resources_kind_known", "community resource and report kind constraints predate phrase packs", true},
		{"user_dictionary_entries", "user_dictionary_entries_kind_check", "user_dictionary_entries kind constraint predates wubi98", true},
		{"", "community_resource_saves_user", "community indexes missing", false},
	})
}

func TestConsoleReadyPreservesMissingMetadataOrder(t *testing.T) {
	checkMissingReadinessMetadataOrder(t, (*Store).consoleReady, []readinessMetadataFixture{
		{"admin_events", "admin_events_kind_check", "admin_events kind constraint", true},
		{"admin_role_permissions", "admin_role_permissions_permission_check", "admin_role_permissions permission constraint", true},
		{"community_plugins", "community_plugins_kind_known", "community_plugins kind constraint", true},
		{"", "release_asset_snapshots_day", "release snapshot day index", false},
		{"", "admin_notifications_target", "notification target index", false},
		{"", "admin_audit_time", "audit time index", false},
	})
}

func checkMissingReadinessMetadataOrder(t *testing.T, readiness func(*Store, context.Context) error, checks []readinessMetadataFixture) {
	t.Helper()
	db := testStore(t)
	ctx := t.Context()
	renamed := map[string]bool{}
	for _, tc := range checks {
		name := pgx.Identifier{tc.name}.Sanitize()
		next := pgx.Identifier{"readiness_" + tc.name}.Sanitize()
		if tc.constraint {
			table := pgx.Identifier{tc.table}.Sanitize()
			if _, err := db.pool.Exec(ctx, `ALTER TABLE `+table+` RENAME CONSTRAINT `+name+` TO `+next); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if !renamed[tc.name] {
					return
				}
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE `+table+` RENAME CONSTRAINT `+next+` TO `+name); err != nil {
					t.Error(err)
				}
			})
		} else {
			if _, err := db.pool.Exec(ctx, `ALTER INDEX `+name+` RENAME TO `+next); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if !renamed[tc.name] {
					return
				}
				if _, err := db.pool.Exec(context.Background(), `ALTER INDEX `+next+` RENAME TO `+name); err != nil {
					t.Error(err)
				}
			})
		}
		renamed[tc.name] = true
	}
	for _, tc := range checks {
		if err := readiness(db, ctx); err == nil || !strings.Contains(err.Error(), tc.missing) {
			t.Fatalf("缺失元数据未按原顺序报告，期望 %s，得到 %v", tc.missing, err)
		}
		old, current := pgx.Identifier{"readiness_" + tc.name}.Sanitize(), pgx.Identifier{tc.name}.Sanitize()
		var sql string
		if tc.constraint {
			sql = `ALTER TABLE ` + pgx.Identifier{tc.table}.Sanitize() + ` RENAME CONSTRAINT ` + old + ` TO ` + current
		} else {
			sql = `ALTER INDEX ` + old + ` RENAME TO ` + current
		}
		if _, err := db.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		renamed[tc.name] = false
	}
	if err := readiness(db, ctx); err != nil {
		t.Fatal("恢复元数据后探测仍失败", err)
	}
}

func TestConsoleReadyMetadataWaitsForMigration(t *testing.T) {
	db := testStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	migration, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer migration.Rollback(context.Background())
	if _, err := migration.Exec(ctx, `SELECT pg_advisory_xact_lock(8372419)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- db.consoleReady(ctx) }()
	waitForAdminDatabaseLock(t, db, migration.Conn().PgConn().PID())
	if err := migration.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("迁移提交后后台元数据检查失败", err)
	}
}
