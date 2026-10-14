package account

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func requireNotificationTargetIndex(t *testing.T, db *Store) {
	t.Helper()
	var present bool
	if err := db.pool.QueryRow(t.Context(), `SELECT EXISTS(
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE i.indrelid='admin_notifications'::regclass AND c.relname='admin_notifications_target'
 AND i.indisvalid AND NOT i.indisunique AND i.indnkeyatts=3
 AND pg_get_indexdef(i.indexrelid,1,true)='kind'
 AND pg_get_indexdef(i.indexrelid,2,true)='target_id'
 AND pg_get_indexdef(i.indexrelid,3,true)='created_at'
)`).Scan(&present); err != nil || !present {
		t.Fatal("通知表缺少非唯一的目标时间索引：", present, err)
	}
}

func TestNotificationTargetIndexUpgrade(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS admin_notifications_target`); err != nil {
		t.Fatal(err)
	}
	other, quoted := communityIndexStoreWithLeadingSchema(t, db)
	// 前置 schema 的无关同名索引不能掩盖实际通知表缺少的索引。
	if _, err := db.pool.Exec(t.Context(), `CREATE TABLE `+quoted+`.unrelated(kind text,target_id text,created_at timestamptz);
 CREATE INDEX admin_notifications_target ON `+quoted+`.unrelated(kind,target_id,created_at)`); err != nil {
		t.Fatal(err)
	}
	if err := other.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "notification target index") {
		t.Fatalf("缺少通知目标索引时未请求迁移：%v", err)
	}
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	requireNotificationTargetIndex(t, db)
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationTargetIndexStartupMigration(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS admin_notifications_target`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOTIFICATION_INDEX_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("NOTIFICATION_INDEX_TEST_PEPPER", strings.Repeat("p", 32))
	a, err := New(t.Context(), Config{Enabled: true, DatabaseEnv: "NOTIFICATION_INDEX_TEST_DB", PepperEnv: "NOTIFICATION_INDEX_TEST_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	requireNotificationTargetIndex(t, db)
}

func TestNotifyOnceTargetLookupUsesBoundedScan(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_notifications CASCADE;
 INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at)
 SELECT 'dict_pr','合成通知','dictpr','target-'||n,now()-interval '30 days' FROM generate_series(1,10000) n;
 ANALYZE admin_notifications;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_notifications", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	if err := a.NotifyOnce(t.Context(), Notification{Kind: NotifyDictPR, TargetPage: "dictpr", TargetID: "target-10000"}); err != nil {
		t.Fatal(err)
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到通知查重查询")
	}
	for _, tc := range []struct {
		name, kind, target string
		wantInsert         int
	}{
		{"existing", NotifyDictPR, "target-10000", 0},
		{"missing", NotifyDictPR, "missing-target", 1},
		{"other-kind", NotifyIssue, "target-10000", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			args := append([]any(nil), query.args...)
			args[0], args[3] = tc.kind, tc.target
			var raw json.RawMessage
			if err := tx.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type node struct {
				Relation string `json:"Relation Name"`
				Rows     int    `json:"Actual Rows"`
				Removed  int    `json:"Rows Removed by Filter"`
				Loops    int    `json:"Actual Loops"`
				Plans    []node `json:"Plans"`
			}
			var plans []struct{ Plan node }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal(err, string(raw))
			}
			inspected, seen := 0, false
			var visit func(node)
			visit = func(n node) {
				if n.Relation == "admin_notifications" && len(n.Plans) == 0 {
					inspected += (n.Rows + n.Removed) * n.Loops
					seen = true
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			if !seen || inspected > 1 {
				t.Errorf("通知查重检查了 %d 行，期望至多一行；找到扫描=%v", inspected, seen)
			}
			var inserted int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM admin_notifications WHERE kind=$1 AND target_id=$2 AND created_at>now()-interval '1 day'`, tc.kind, tc.target).Scan(&inserted); err != nil || inserted != tc.wantInsert {
				t.Fatalf("通知去重语义错误：插入 %d 行，期望 %d，错误 %v", inserted, tc.wantInsert, err)
			}
		})
	}
}
