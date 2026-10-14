package account

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func requireReleaseSnapshotDayIndex(t *testing.T, db *Store) {
	t.Helper()
	var present bool
	if err := db.pool.QueryRow(t.Context(), `SELECT EXISTS(
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE i.indrelid='release_asset_snapshots'::regclass AND c.relname='release_asset_snapshots_day'
 AND i.indisvalid AND i.indnkeyatts=1 AND pg_get_indexdef(i.indexrelid,1,true)='day'
)`).Scan(&present); err != nil || !present {
		t.Fatal("下载快照缺少日期索引：", present, err)
	}
}

func TestReleaseSnapshotDayIndexUpgrade(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS release_asset_snapshots_day`); err != nil {
		t.Fatal(err)
	}
	other, quoted := communityIndexStoreWithLeadingSchema(t, db)
	// 前置 schema 的同名索引不能掩盖实际业务表缺少的索引。
	if _, err := db.pool.Exec(t.Context(), `CREATE TABLE `+quoted+`.unrelated(day date);
 CREATE INDEX release_asset_snapshots_day ON `+quoted+`.unrelated(day)`); err != nil {
		t.Fatal(err)
	}
	if err := other.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "release snapshot day index") {
		t.Fatalf("缺少下载快照日期索引时未请求迁移：%v", err)
	}
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	requireReleaseSnapshotDayIndex(t, db)
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseSnapshotDayIndexStartupMigration(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS release_asset_snapshots_day`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELEASE_SNAPSHOT_INDEX_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("RELEASE_SNAPSHOT_INDEX_TEST_PEPPER", strings.Repeat("p", 32))
	a, err := New(t.Context(), Config{Enabled: true, DatabaseEnv: "RELEASE_SNAPSHOT_INDEX_TEST_DB", PepperEnv: "RELEASE_SNAPSHOT_INDEX_TEST_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	requireReleaseSnapshotDayIndex(t, db)
}

func TestReleaseSnapshotLatestDayUsesBoundedScan(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE release_asset_snapshots;
 INSERT INTO release_asset_snapshots(repo,tag,asset,day,download_count)
 SELECT 'owner/example','windows-v1','asset-'||n||'.exe',date '2026-10-14'-age,1000-age
 FROM generate_series(1,100) n CROSS JOIN generate_series(0,99) age;
 ANALYZE release_asset_snapshots;`); err != nil {
		t.Fatal(err)
	}
	const query = `SELECT to_char(max(day),'YYYY-MM-DD') FROM release_asset_snapshots`
	var latest string
	if err := db.pool.QueryRow(t.Context(), query).Scan(&latest); err != nil || latest != "2026-10-14" {
		t.Fatal("最新快照日期错误：", latest, err)
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Rows     int    `json:"Actual Rows"`
		Loops    int    `json:"Actual Loops"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	returnedRows := 0
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "release_asset_snapshots" {
			returnedRows += n.Rows * n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if returnedRows != 1 {
		t.Fatalf("最新日期查询读取了 %d 条快照，期望只取一条", returnedRows)
	}
}
