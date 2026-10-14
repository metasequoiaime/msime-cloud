package account

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminUserSessionStatisticsShareActivityScan(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "admin-session-owner@example.test"})
	other := complete(t, db, Identity{"email", "admin-session-other@example.test"})
	empty := complete(t, db, Identity{"email", "admin-session-empty@example.test"})
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM auth_sessions WHERE user_id=ANY($1::text[])`, []string{owner.User.ID, empty.User.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_sessions(id,user_id,access_hash,refresh_hash,created_at,access_expires,expires_at,revoked)
 SELECT 'admin-stat-session-'||n,$1,md5('admin-stat-access-'||n),md5('admin-stat-refresh-'||n),now()-n*interval '1 minute',now()+interval '15 minutes'-n*interval '1 minute',
 CASE WHEN n%4 IN (0,3) THEN now()-interval '1 day' ELSE now()+interval '1 day' END,n%4 IN (0,2) FROM generate_series(1,60) n`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	var lastActive time.Time
	if err := db.pool.QueryRow(t.Context(), `SELECT created_at FROM auth_sessions WHERE id='admin-stat-session-1'`).Scan(&lastActive); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "auth_users u", queries: make(chan communityPageQuery, 4)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	for _, user := range []string{owner.User.ID, empty.User.ID} {
		t.Run(user, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.adminUser(w, httptest.NewRequest("GET", "/api/users/"+user, nil), user)
			var item struct {
				ID         string     `json:"id"`
				Active     int        `json:"active_sessions"`
				Total      int        `json:"total_sessions"`
				LastActive *time.Time `json:"last_active"`
				Sessions   []struct {
					ID string `json:"id"`
				} `json:"sessions"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil || item.ID != user {
				t.Fatal(w.Code, w.Body.String())
			}
			if user == owner.User.ID {
				if item.Active != 15 || item.Total != 60 || len(item.Sessions) != 50 || item.LastActive == nil || !item.LastActive.Equal(lastActive) {
					t.Fatalf("会话统计错误：%+v", item)
				}
			} else if item.Active != 0 || item.Total != 0 || len(item.Sessions) != 0 || item.LastActive != nil {
				t.Fatalf("零会话统计错误：%+v", item)
			}
			var query communityPageQuery
			select {
			case query = <-trace.queries:
			default:
				t.Fatal("没有记录到用户详情查询")
			}
			var raw json.RawMessage
			if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type node struct {
				Relation string `json:"Relation Name"`
				Plans    []node `json:"Plans"`
			}
			var plans []struct{ Plan node }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal(err, string(raw))
			}
			scans := 0
			var visit func(node)
			visit = func(n node) {
				if n.Relation == "auth_sessions" {
					scans++
				}
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			// 活动与计数共用一次扫描，最近会话和审核历史分别保留独立扫描。
			if scans != 3 {
				t.Errorf("会话表扫描了 %d 次，期望 3 次", scans)
			}
		})
	}
	if _, err := db.Authenticate(t.Context(), other.AccessToken); err != nil {
		t.Fatal("其他账号的会话应保持有效：", err)
	}
}
