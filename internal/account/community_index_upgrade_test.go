package account

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func communityIndexStoreWithLeadingSchema(t *testing.T, db *Store) (*Store, string) {
	t.Helper()
	name := "community_index_" + randomToken()[:12]
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := db.pool.Exec(t.Context(), `CREATE SCHEMA `+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `DROP SCHEMA `+quoted+` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	var searchPath string
	if err := db.pool.QueryRow(t.Context(), `SHOW search_path`).Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	cfg := db.pool.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = quoted + "," + searchPath
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Store{pool: pool}, quoted
}

func TestCommunityIndexesWithLeadingSchema(t *testing.T) {
	db := testStore(t)
	other, _ := communityIndexStoreWithLeadingSchema(t, db)
	// 前置 schema 没有业务表，Ready 应检查 search_path 后面的实际表。
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal("已有索引被误判为缺失：", err)
	}
}

func TestCommunitySkinOwnerIndexSchemaIsolation(t *testing.T) {
	db := testStore(t)
	other, quoted := communityIndexStoreWithLeadingSchema(t, db)
	// 前置 schema 的其他表与业务表各有一个同名索引，迁移只能删除业务表上的索引。
	if _, err := db.pool.Exec(t.Context(), `CREATE INDEX community_skins_owner ON community_skins(owner_id,created_at DESC);
CREATE TABLE `+quoted+`.unrelated (owner_id text);
CREATE INDEX community_skins_owner ON `+quoted+`.unrelated(owner_id)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := other.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var skinRemoved, unrelatedKept bool
	if err := other.pool.QueryRow(t.Context(), `SELECT
 NOT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='community_skins'::regclass AND c.relname='community_skins_owner'),
 EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=$1::regclass AND c.relname='community_skins_owner')`, quoted+`.unrelated`).Scan(&skinRemoved, &unrelatedKept); err != nil || !skinRemoved || !unrelatedKept {
		t.Fatal("皮肤索引删除或同名索引隔离失败：", skinRemoved, unrelatedKept, err)
	}
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCommunityIndexStartupMigration(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX community_resources_owner`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMUNITY_INDEX_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("COMMUNITY_INDEX_TEST_PEPPER", strings.Repeat("p", 32))
	a, err := New(t.Context(), Config{Enabled: true, DatabaseEnv: "COMMUNITY_INDEX_TEST_DB", PepperEnv: "COMMUNITY_INDEX_TEST_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal("启动后索引仍缺失：", err)
	}
}

func TestCommunityIndexUpgrade(t *testing.T) {
	db := testStore(t)
	indexes := []string{
		"community_resource_saves_user", "community_resources_owner",
		"community_skin_downloads_user", "community_skin_ratings_user",
		"community_candidate_skin_downloads_user", "community_candidate_skin_ratings_user",
		"community_plugin_downloads_user", "community_plugin_ratings_user",
		"community_resource_ratings_user", "community_reports_reporter", "community_reports_item_newest",
	}
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 逐个模拟已部署的旧库，确认任一索引缺失都会触发启动迁移。
	for _, name := range indexes {
		t.Run(name, func(t *testing.T) {
			if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS `+name); err != nil {
				t.Fatal(err)
			}
			if err := db.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "community indexes") {
				t.Fatalf("缺少索引时未请求迁移：%v", err)
			}
			for range 2 {
				if err := db.Migrate(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Ready(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommunitySkinRedundantOwnerIndexUpgrade(t *testing.T) {
	db := testStore(t)
	// 模拟先前版本加上的重复作者索引；原有 UNIQUE(owner_id,id) 应继续保留。
	if _, err := db.pool.Exec(t.Context(), `CREATE INDEX IF NOT EXISTS community_skins_owner ON community_skins(owner_id,created_at DESC)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "community indexes") {
		t.Fatalf("重复索引未触发迁移：%v", err)
	}
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var removed, ownerUnique bool
	if err := db.pool.QueryRow(t.Context(), `SELECT to_regclass('community_skins_owner') IS NULL, EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='community_skins'::regclass AND contype='u' AND pg_get_constraintdef(oid)='UNIQUE (owner_id, id)')`).Scan(&removed, &ownerUnique); err != nil || !removed || !ownerUnique {
		t.Fatal(removed, ownerUnique, err)
	}
	if err := db.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}
