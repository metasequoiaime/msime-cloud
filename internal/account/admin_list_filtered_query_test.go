package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminFilteredPlan struct {
	Filter   string              `json:"Filter"`
	Relation string              `json:"Relation Name"`
	Rows     int                 `json:"Actual Rows"`
	Loops    int                 `json:"Actual Loops"`
	Removed  int                 `json:"Rows Removed by Filter"`
	Plans    []adminFilteredPlan `json:"Plans"`
}

func adminFilteredExplain(t *testing.T, db *Store, query communityPageQuery, mode string) adminFilteredPlan {
	t.Helper()
	tx, err := db.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	for _, sql := range []string{`SET LOCAL work_mem='64kB'`, `SET LOCAL plan_cache_mode=` + mode} {
		if _, err := tx.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	const statement = "admin_filtered_query"
	if _, err := tx.Conn().Prepare(t.Context(), statement, query.sql); err != nil {
		t.Fatal(err)
	}
	defer tx.Conn().Deallocate(t.Context(), statement)
	params := make([]string, len(query.args))
	for i := range params {
		params[i] = fmt.Sprintf("$%d", i+1)
	}
	// EXPLAIN EXECUTE 才会使用原查询的缓存计划；简单协议负责把测试参数安全地编码为 SQL 字面量。
	explain := `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE ` + statement + "(" + strings.Join(params, ",") + ")"
	args := append([]any{pgx.QueryExecModeSimpleProtocol}, query.args...)
	var raw json.RawMessage
	if err := tx.QueryRow(t.Context(), explain, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var generic, custom int
	if err := tx.QueryRow(t.Context(), `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`, statement).Scan(&generic, &custom); err != nil {
		t.Fatal(err)
	}
	if (mode == "force_generic_plan" && (generic != 1 || custom != 0)) || (mode == "force_custom_plan" && (custom != 1 || generic != 0)) {
		t.Fatalf("计划模式没有生效：%s generic=%d custom=%d", mode, generic, custom)
	}
	var plans []struct{ Plan adminFilteredPlan }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	return plans[0].Plan
}

func adminFilteredService(t *testing.T, db *Store, table string) (*Service, *communityPaginationTrace) {
	t.Helper()
	trace := &communityPaginationTrace{table: table, queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Service{store: &Store{pool: pool}}, trace
}

func TestAdminFilteredListUsesConcretePredicates(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_events;
 INSERT INTO admin_events(id,kind,platform,version,created_at)
 SELECT 'filtered-page-'||lpad(n::text,5,'0'),'download',CASE WHEN n<=10 THEN 'rare' WHEN n%10=0 THEN 'ios' ELSE 'android' END,'filtered-page',
 '2026-01-01'::timestamptz+((n+1)/2)*interval '1 second' FROM generate_series(1,5000) n;
 ANALYZE admin_events`); err != nil {
		t.Fatal(err)
	}
	a, trace := adminFilteredService(t, db, "admin_events")
	baseline := &Service{store: db}
	for _, tc := range []struct {
		platform    string
		page, total int
	}{
		{"android", 1, 4491}, {"android", 2, 4491}, {"android", 90, 4491}, {"android", 91, 4491},
		{"ios", 1, 499}, {"ios", 10, 499}, {"ios", 11, 499}, {"rare", 1, 10}, {"rare", 2, 10}, {"missing", 1, 0},
	} {
		t.Run(fmt.Sprintf("%s/page=%d", tc.platform, tc.page), func(t *testing.T) {
			path := "/api/downloads?platform=" + tc.platform
			w := httptest.NewRecorder()
			a.adminList(w, httptest.NewRequest("GET", path, nil), downloadsList, tc.page, "")
			old := httptest.NewRecorder()
			baseline.adminList(old, httptest.NewRequest("GET", path, nil), downloadsList, tc.page, "filtered-page")
			var got, want any
			if w.Code != 200 || old.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || json.Unmarshal(old.Body.Bytes(), &want) != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("筛选分页与全匹配搜索结果不同：", w.Code, w.Body.String(), old.Code, old.Body.String())
			}
			var body struct {
				Items       []struct{ ID string }
				Total, Page int
				More        bool `json:"has_more"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantRows := max(0, min(50, tc.total-(tc.page-1)*50))
			if body.Items == nil || len(body.Items) != wantRows || body.Total != tc.total || body.Page != tc.page || body.More != (tc.page*50 < tc.total) {
				t.Fatal(w.Body.String())
			}
			ids := []string{}
			for n := 5000; n >= 1; n-- {
				if (tc.platform == "android" && n > 10 && n%10 != 0) || (tc.platform == "ios" && n > 10 && n%10 == 0) || (tc.platform == "rare" && n <= 10) {
					ids = append(ids, fmt.Sprintf("filtered-page-%05d", n))
				}
			}
			for i, item := range body.Items {
				if item.ID != ids[(tc.page-1)*50+i] {
					t.Fatal("同时间条目的 ID 次序错误：", item)
				}
			}
			query := <-trace.queries
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				t.Run(mode, func(t *testing.T) {
					plan := adminFilteredExplain(t, db, query, mode)
					allWork := 0
					var visit func(adminFilteredPlan)
					visit = func(n adminFilteredPlan) {
						if n.Relation == "admin_events" {
							allWork += (n.Rows + n.Removed) * n.Loops
							if strings.Contains(n.Filter, " OR ") {
								t.Errorf("行级筛选保留了可选条件：%s", n.Filter)
							}
						}
						for _, child := range n.Plans {
							visit(child)
						}
					}
					visit(plan)
					if allWork > 5000+wantRows {
						t.Errorf("筛选读取了 %d 条事件，期望单次扫描及页内详情", allWork)
					}

				})
			}
		})
	}
}

func TestAdminRoleFilterAvoidsRepeatedCalculation(t *testing.T) {
	db, _ := usersTestStore(t)
	for _, q := range []string{
		`INSERT INTO auth_users(id,display_name,created_at) SELECT 'filtered-role-'||n,'合成账号',now()+n*interval '1 second' FROM generate_series(1,2000) n`,
		`INSERT INTO auth_identities(provider,subject,user_id) SELECT 'email','filtered-role-'||n||'@example.test','filtered-role-'||n FROM generate_series(1,2000) n`,
		`INSERT INTO admin_members(email,role) SELECT 'filtered-role-'||n||'@example.test','reviewer' FROM generate_series(1,10) n`,
	} {
		if _, err := db.pool.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `DELETE FROM admin_members WHERE email LIKE 'filtered-role-%@example.test'`); err != nil {
			t.Error(err)
		}
	})
	a, trace := adminFilteredService(t, db, "auth_users u")
	a.admin.Owners = []string{"filtered-role-2000@example.test"}
	for _, role := range []string{"reviewer", "maintainer", "missing"} {
		t.Run(role, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.adminList(w, httptest.NewRequest("GET", "/api/users?role="+role, nil), usersList, 1, "")
			var body struct {
				Items []struct{ ID, Role string }
				Total int
			}
			wantRows := map[string]int{"reviewer": 10, "maintainer": 1}[role]
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Items == nil || len(body.Items) != wantRows || body.Total != wantRows {
				t.Fatal(w.Code, w.Body.String())
			}
			for _, item := range body.Items {
				if item.Role != role || (role == "maintainer" && item.ID != "filtered-role-2000") {
					t.Fatal(item)
				}
			}
			query := <-trace.queries
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				t.Run(mode, func(t *testing.T) {
					plan := adminFilteredExplain(t, db, query, mode)
					roleLoops, detailLoops := 0, 0
					var visit func(adminFilteredPlan)
					visit = func(n adminFilteredPlan) {
						if n.Relation == "admin_members" {
							roleLoops += n.Loops
						}
						if n.Relation == "auth_sessions" {
							detailLoops += n.Loops
						}
						for _, child := range n.Plans {
							visit(child)
						}
					}
					visit(plan)
					if roleLoops < 1999 || roleLoops > 2000+wantRows {
						t.Errorf("角色查询执行 %d 次，期望只筛选一遍并读取页内详情", roleLoops)
					}
					if detailLoops != wantRows {
						t.Errorf("会话统计执行 %d 次，期望页内 %d 次", detailLoops, wantRows)
					}
				})
			}
		})
	}
}
