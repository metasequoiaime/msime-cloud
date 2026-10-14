package account

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminUserRoleStatsUseBoundedScans(t *testing.T) {
	db, a := usersTestStore(t)
	stats := func(service *Service) map[string]int64 {
		t.Helper()
		w := usersCall(service, usersOwner, "GET", "/api/users/stats", "")
		var body struct {
			Roles map[string]int64 `json:"roles"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("用户统计请求失败：%d %s", w.Code, w.Body.String())
		}
		return body.Roles
	}
	if roles := stats(a); len(roles) != 0 {
		t.Fatalf("空表角色统计错误：%v", roles)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id) SELECT 'role-user-'||n FROM generate_series(1,100) n;
 INSERT INTO auth_users(id) VALUES('role-no-identity');
 INSERT INTO auth_identities(provider,subject,user_id) SELECT 'email','role-user-'||n||'@example.test','role-user-'||n FROM generate_series(1,100) n;
 INSERT INTO admin_members(email,role) SELECT 'role-user-'||n||'@example.test',CASE n%3 WHEN 0 THEN 'maintainer' WHEN 1 THEN 'operator' ELSE 'reviewer' END FROM generate_series(1,20) n;
 INSERT INTO admin_roles(key,name) VALUES('stats_alpha','合成角色甲'),('stats_zeta','合成角色乙') ON CONFLICT(key) DO NOTHING;
 INSERT INTO admin_members(email,role,enabled) VALUES('disabled@example.test','maintainer',false),('view@example.test','readonly',true),('alpha@example.test','stats_alpha',true),('zeta@example.test','stats_zeta',true);
 INSERT INTO auth_identities(provider,subject,user_id,email,email_verified) VALUES
 ('google','role-priority','role-user-2','ROLE-USER-3@example.test',true),
 ('google','role-unverified','role-user-21','role-user-3@example.test',false),
 ('apple','role-verified','role-user-22','role-user-2@example.test',true),
 ('google','role-empty','role-user-23','',true),
 ('google','role-disabled','role-user-24','disabled@example.test',true),
 ('google','role-owner','role-user-25','verified-owner@example.test',true),
 ('google','role-custom-z','role-user-26','zeta@example.test',true),
 ('apple','role-custom-a','role-user-26','alpha@example.test',true),
 ('google','role-readonly','role-user-27','view@example.test',true),
 ('google','role-duplicate-g','role-user-28','role-user-2@example.test',true),
 ('apple','role-duplicate-a','role-user-28','role-user-2@example.test',true);
 ANALYZE auth_users,auth_identities,admin_members;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `DELETE FROM admin_members WHERE role IN ('stats_alpha','stats_zeta'); DELETE FROM admin_roles WHERE key IN ('stats_alpha','stats_zeta')`); err != nil {
			t.Error(err)
		}
	})
	trace := &communityPaginationTrace{table: "auth_users", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, owners := range [][]string{{}, {"role-user-1@example.test", "verified-owner@example.test"}} {
		service := &Service{store: &Store{pool: pool}}
		service.ConfigureAdmin(AdminSettings{Owners: owners})
		got := stats(service)
		// 列表及详情沿用逐用户规则，以它为参照验证批量统计的身份校验和角色优先级。
		rows, err := db.pool.Query(t.Context(), `SELECT role,count(*) FROM (SELECT `+userRoleSQL("$1")+` AS role FROM auth_users u) x GROUP BY role`, owners)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int64{}
		for rows.Next() {
			var role string
			var count int64
			if err := rows.Scan(&role, &count); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			want[role] = count
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("角色统计不符合现有规则：got=%v want=%v owners=%v", got, want, owners)
		}
		var query communityPageQuery
		select {
		case query = <-trace.queries:
		default:
			t.Fatal("没有记录到角色统计查询")
		}
		var raw json.RawMessage
		if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		type node struct {
			Relation string `json:"Relation Name"`
			Loops    int    `json:"Actual Loops"`
			Plans    []node `json:"Plans"`
		}
		var plans []struct{ Plan node }
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatal(err, string(raw))
		}
		scans := map[string]int{}
		var visit func(node)
		visit = func(n node) {
			scans[n.Relation] += n.Loops
			for _, child := range n.Plans {
				visit(child)
			}
		}
		visit(plans[0].Plan)
		for _, relation := range []string{"auth_identities", "admin_members"} {
			if scans[relation] > 1 {
				t.Errorf("角色统计对 %s 扫描 %d 次，不应按用户重复扫描", relation, scans[relation])
			}
		}
	}
}
