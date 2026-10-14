package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type auditPageTrace struct {
	queries chan communityPageQuery
}

func (trace *auditPageTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM admin_audit") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*auditPageTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func requireAuditTimeIndex(t *testing.T, db *Store) {
	t.Helper()
	var present bool
	if err := db.pool.QueryRow(t.Context(), `SELECT EXISTS(
 SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 WHERE i.indrelid='admin_audit'::regclass AND c.relname='admin_audit_time'
 AND i.indisvalid AND NOT i.indisunique AND i.indnkeyatts=2
 AND pg_get_indexdef(i.indexrelid,1,true)='created_at'
 AND pg_get_indexdef(i.indexrelid,2,true)='id'
)`).Scan(&present); err != nil || !present {
		t.Fatal("审计表缺少时间和 ID 排序索引：", present, err)
	}
}

func TestAuditTimeIndexUpgrade(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS admin_audit_time`); err != nil {
		t.Fatal(err)
	}
	other, quoted := communityIndexStoreWithLeadingSchema(t, db)
	// 前置 schema 的无关同名索引不能掩盖实际审计表缺少的索引。
	if _, err := db.pool.Exec(t.Context(), `CREATE TABLE `+quoted+`.unrelated(created_at timestamptz,id bigint);
 CREATE INDEX admin_audit_time ON `+quoted+`.unrelated(created_at,id)`); err != nil {
		t.Fatal(err)
	}
	if err := other.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "audit time index") {
		t.Fatalf("缺少审计时间索引时未请求迁移：%v", err)
	}
	for range 2 {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	requireAuditTimeIndex(t, db)
	if err := other.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditTimeIndexStartupMigration(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `DROP INDEX IF EXISTS admin_audit_time`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_INDEX_TEST_DB", os.Getenv("MSIME_TEST_DATABASE_URL"))
	t.Setenv("AUDIT_INDEX_TEST_PEPPER", strings.Repeat("p", 32))
	a, err := New(t.Context(), Config{Enabled: true, DatabaseEnv: "AUDIT_INDEX_TEST_DB", PepperEnv: "AUDIT_INDEX_TEST_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	requireAuditTimeIndex(t, db)
}

func TestAuditListPageSelectionUsesBoundedScan(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit;
 INSERT INTO admin_audit(action,target,actor,detail,created_at)
 SELECT 'admin_pref_set','target-'||n,'pat:synthetic@example.test',jsonb_build_object('value',n),
 '2026-01-01'::timestamptz+((n+1)/2)*interval '1 second' FROM generate_series(1,10000) n;
 ANALYZE admin_audit;`); err != nil {
		t.Fatal(err)
	}
	var lastID int64
	if err := db.pool.QueryRow(t.Context(), `SELECT max(id) FROM admin_audit`).Scan(&lastID); err != nil {
		t.Fatal(err)
	}
	trace := &auditPageTrace{queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, page := range []int{1, 2, 200, 201} {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			w := httptest.NewRecorder()
			a.AdminHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/audit?page=%d", page), nil))
			var body struct {
				Items []struct {
					ID     int64          `json:"id"`
					Target string         `json:"target"`
					Detail map[string]int `json:"detail"`
				} `json:"items"`
				Total int  `json:"total"`
				Page  int  `json:"page"`
				More  bool `json:"has_more"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Total != 10000 || body.Page != page || body.More != (page < 200) || len(body.Items) != min(50, max(0, 10000-(page-1)*50)) {
				t.Fatalf("审计分页结果错误：%d %s", w.Code, w.Body.String())
			}
			for i, item := range body.Items {
				position := (page-1)*50 + i
				if item.ID != lastID-int64(position) || item.Target != fmt.Sprintf("target-%d", 10000-position) || item.Detail["value"] != 10000-position {
					t.Fatalf("同时间戳的 ID 排序或审计详情错误：%+v", item)
				}
			}
			var query communityPageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到审计列表查询")
			}
			var raw json.RawMessage
			if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type node struct {
				Relation string `json:"Relation Name"`
				Subplan  string `json:"Subplan Name"`
				Rows     int    `json:"Actual Rows"`
				Loops    int    `json:"Actual Loops"`
				Plans    []node `json:"Plans"`
			}
			var plans []struct{ Plan node }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal(err, string(raw))
			}
			pageRows, seen := 0, false
			var visit func(node, bool)
			visit = func(n node, selected bool) {
				selected = selected || n.Subplan == "CTE selected"
				if selected && n.Relation == "admin_audit" {
					pageRows += n.Rows * n.Loops
					seen = true
				}
				for _, child := range n.Plans {
					visit(child, selected)
				}
			}
			visit(plans[0].Plan, false)
			// 总数查询仍完整计数，这里只限制选取当前页时读取的行数，允许 OFFSET 跳过前页。
			if !seen || pageRows > page*50 {
				t.Fatalf("审计选页读取了 %d 行，最多需要 %d 行；找到选页扫描=%v", pageRows, page*50, seen)
			}
		})
	}
}
