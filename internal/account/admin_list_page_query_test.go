package account

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminListUserDetailsRunOnlyForPage(t *testing.T) {
	db := testStore(t)
	for _, query := range []string{
		`INSERT INTO auth_users(id,display_name,created_at) SELECT 'list-user-'||n,'合成账号',now()+n*interval '1 second' FROM generate_series(1,121) n`,
		`INSERT INTO auth_identities(provider,subject,user_id) SELECT 'email','list-user-'||n||'@example.test','list-user-'||n FROM generate_series(1,121) n`,
		`INSERT INTO auth_sessions(id,user_id,access_hash,refresh_hash,access_expires,expires_at,revoked) SELECT 'list-session-'||u||'-'||n,'list-user-'||u,md5('list-access-'||u||'-'||n),md5('list-refresh-'||u||'-'||n),now()+interval '15 minutes',now()+interval '1 day',n%3=0 FROM generate_series(1,121) u CROSS JOIN generate_series(1,10) n`,
	} {
		if _, err := db.pool.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	trace := &communityPaginationTrace{table: "auth_users u", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for page := 1; page <= 4; page++ {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/users", nil)
			a.adminList(w, r, usersList, page, "", r.URL.Query())
			var result struct {
				Items []struct {
					ID       string `json:"id"`
					Devices  int    `json:"devices"`
					Sessions int    `json:"sessions"`
					Contact  string `json:"contact"`
				} `json:"items"`
				Page    int  `json:"page"`
				Total   int  `json:"total"`
				HasMore bool `json:"has_more"`
			}
			wantRows := max(0, min(50, 121-(page-1)*50))
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != wantRows || result.Total != 121 || result.Page != page || result.HasMore != (page < 3) {
				t.Fatal(w.Code, w.Body.String())
			}
			for i, item := range result.Items {
				wantID := fmt.Sprintf("list-user-%d", 121-(page-1)*50-i)
				if item.ID != wantID || item.Devices != 7 || item.Sessions != 7 || item.Contact != "l***@example.test" {
					t.Fatalf("列表条目错误：%+v，期望账号 %s", item, wantID)
				}
			}
			var query communityPageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到列表查询")
			}
			for _, fixed := range []bool{false, true} {
				t.Run(fmt.Sprintf("fixed=%t", fixed), func(t *testing.T) {
					tx, err := db.pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					if fixed {
						// 固定连接顺序，确保统计不会因优化器重排而先于页内 ID 筛选执行。
						if _, err := tx.Exec(t.Context(), `SET LOCAL join_collapse_limit=1`); err != nil {
							t.Fatal(err)
						}
					}
					var raw json.RawMessage
					if err := tx.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					type node struct {
						Relation string `json:"Relation Name"`
						Parent   string `json:"Parent Relationship"`
						Loops    int    `json:"Actual Loops"`
						Rows     int    `json:"Actual Rows"`
						Plans    []node `json:"Plans"`
					}
					var plans []struct{ Plan node }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatal(err, string(raw))
					}
					loops := 0
					identityRows := 0
					var visit func(node)
					visit = func(n node) {
						// 角色子查询也读取身份表，排除它们以单独检查联系人连接。
						if n.Parent == "SubPlan" {
							return
						}
						if n.Relation == "auth_sessions" {
							loops += n.Loops
						}
						if n.Relation == "auth_identities" {
							identityRows += n.Rows * n.Loops
						}
						for _, child := range n.Plans {
							visit(child)
						}
					}
					visit(plans[0].Plan)
					if loops != wantRows {
						t.Errorf("会话统计执行了 %d 次，期望页内 %d 次", loops, wantRows)
					}
					if identityRows != wantRows {
						t.Errorf("联系人读取了 %d 条身份，期望页内 %d 条", identityRows, wantRows)
					}
				})
			}
		})
	}
}
