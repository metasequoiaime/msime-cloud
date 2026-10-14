package account

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminListScalarFilterTypes(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	for name, list := range adminLists {
		if len(list.filters) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			fields := make([]string, 0, len(list.filters))
			for _, f := range list.filters {
				fields = append(fields, "x."+f.field)
			}
			args := []any{"", 0, 1}
			if list.args != nil {
				args = append(args, list.args(a)...)
			}
			rows, err := db.pool.Query(t.Context(), `SELECT `+strings.Join(fields, ",")+` FROM (`+list.query+`) x WHERE $1::text='' AND $2::int=0 AND $3::int=1 LIMIT 0`, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			// 直接转文本与原 JSON 提取对文本、布尔值等价；新增其他类型时必须重新验证语义。
			for _, field := range rows.FieldDescriptions() {
				if field.DataTypeOID != pgtype.TextOID && field.DataTypeOID != pgtype.BoolOID {
					t.Errorf("筛选字段 %s 的类型 %d 需要验证文本转换语义", field.Name, field.DataTypeOID)
				}
			}
		})
	}
}

func TestAdminListScalarFiltersUseExistingIndexes(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id) VALUES('scalar-owner');
 INSERT INTO community_skins(id,owner_id,name,design,category,moderation,created_at)
 SELECT 'scalar-'||n,'scalar-owner','合成皮肤','{}',CASE WHEN n%100=0 THEN 'tech' ELSE 'other' END,
 CASE WHEN n%100=0 THEN 'pending' ELSE 'approved' END,'2026-01-01'::timestamptz+n*interval '1 second'
 FROM generate_series(1,20000) n;
 ANALYZE community_skins; ANALYZE auth_users;`); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "community_skins s", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, params := range []string{"category=tech", "status=pending", "category=tech&status=pending"} {
		for _, page := range []int{1, 4, 5} {
			t.Run(fmt.Sprintf("%s/page=%d", params, page), func(t *testing.T) {
				w := httptest.NewRecorder()
				a.AdminHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/skins?%s&page=%d", params, page), nil))
				var body struct {
					Items []struct {
						ID       string `json:"id"`
						Category string `json:"category"`
						Status   string `json:"moderation"`
					} `json:"items"`
					Total int  `json:"total"`
					Page  int  `json:"page"`
					More  bool `json:"has_more"`
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Total != 200 || body.Page != page || body.More != (page < 4) || len(body.Items) != min(50, max(0, 200-(page-1)*50)) {
					t.Fatal(w.Code, w.Body.String())
				}
				for i, item := range body.Items {
					if item.ID != fmt.Sprintf("scalar-%d", 20000-((page-1)*50+i)*100) || item.Category != "tech" || item.Status != "pending" {
						t.Fatal("筛选结果或排序错误：", item)
					}
				}
				var query communityPageQuery
				select {
				case query = <-trace.queries:
				default:
					t.Fatal("没有记录到筛选列表查询")
				}
				var raw json.RawMessage
				if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				type node struct {
					Relation string `json:"Relation Name"`
					Subplan  string `json:"Subplan Name"`
					Rows     int    `json:"Actual Rows"`
					Removed  int    `json:"Rows Removed by Filter"`
					Loops    int    `json:"Actual Loops"`
					Plans    []node `json:"Plans"`
				}
				var plans []struct{ Plan node }
				if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
					t.Fatal(err, string(raw))
				}
				scanned, seen := 0, false
				var visit func(node, bool)
				visit = func(n node, filtered bool) {
					filtered = filtered || n.Subplan == "CTE filtered"
					if filtered && n.Relation == "community_skins" {
						scanned += (n.Rows + n.Removed) * n.Loops
						seen = true
					}
					for _, child := range n.Plans {
						visit(child, filtered)
					}
				}
				visit(plans[0].Plan, false)
				// 精确总数需要读取全部 200 条匹配记录，但不应读取另外 19800 条。
				if !seen || scanned > 200 {
					t.Fatalf("筛选读取了 %d 条皮肤记录，期望至多 200 条；找到扫描=%v", scanned, seen)
				}
			})
		}
	}
}

func TestAdminListScalarFilterTextMatching(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit`); err != nil {
		t.Fatal(err)
	}
	actors := []string{"pat:合成\"账号\\Admin@example.test", "pat:another@example.test"}
	for _, actor := range actors {
		if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_audit(actor,action,target) VALUES($1,$2,'合成目标')`, actor, "quoted\"\\action"); err != nil {
			t.Fatal(err)
		}
	}
	a := &Service{store: db}
	for _, tc := range []struct {
		params url.Values
		total  int
	}{
		{url.Values{"action": {"quoted\"\\action"}}, 2},
		{url.Values{"action": {"quoted"}}, 0},
		{url.Values{"actor": {"合成\"账号\\admin"}}, 0},
		{url.Values{"actor": {"合成\"账号\\\\admin"}}, 1},
		{url.Values{"actor": {"%@example.test"}}, 2},
		{url.Values{"actor": {"ANOTHER"}, "action": {"quoted\"\\action"}}, 1},
	} {
		for _, search := range []string{"", "合成目标"} {
			params := maps.Clone(tc.params)
			params.Set("q", search)
			w := httptest.NewRecorder()
			a.AdminHTTP(w, httptest.NewRequest("GET", "/api/audit?"+params.Encode(), nil))
			var body struct {
				Total int               `json:"total"`
				Items []json.RawMessage `json:"items"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Total != tc.total || len(body.Items) != tc.total {
				t.Fatal(params, w.Code, w.Body.String())
			}
		}
	}
}
