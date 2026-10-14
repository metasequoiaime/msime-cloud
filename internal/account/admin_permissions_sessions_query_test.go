package account

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminPermissionsMemberSessionsUseOneScan(t *testing.T) {
	db := testStore(t)
	truncateAdminPersonal(t, db)
	latest := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_members(email,role,enabled) VALUES
 ('owner@example.test','readonly',true),('helper@example.test','reviewer',true),
 ('off@example.test','operator',false),('empty@example.test','readonly',true),('expired@example.test','reviewer',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_sessions(token_hash,subject,email,expires_at,last_seen_at) VALUES
 ('helper-active-1','helper','helper@example.test',now()+interval '1 day',$1::timestamptz-interval '2 hours'),
 ('helper-active-2','helper','helper@example.test',now()+interval '1 day',$1::timestamptz-interval '1 hour'),
 ('helper-expired','helper','helper@example.test',now()-interval '1 day',$1),
 ('off-active','off','off@example.test',now()+interval '1 day',$1),
 ('expired-only','expired','expired@example.test',now()-interval '1 day',$1)`, latest); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "admin_members m", queries: make(chan communityPageQuery, 1)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}, admin: AdminSettings{Owners: []string{"owner@example.test"}}}
	w := permissionsRequest(a, usersOwner, "GET", "")
	var result permissionsResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Members) != 5 {
		t.Fatal(w.Code, w.Body.String())
	}
	for i, want := range []struct {
		email    string
		sessions int64
		seen     bool
	}{
		{"owner@example.test", 0, false},
		{"empty@example.test", 0, false},
		{"expired@example.test", 0, true},
		{"helper@example.test", 2, true},
		{"off@example.test", 1, true},
	} {
		got := result.Members[i]
		if got.Email != want.email || got.Sessions != want.sessions || (got.LastSeenAt != nil) != want.seen || (want.seen && !got.LastSeenAt.Equal(latest)) || got.Owner != (i == 0) || (i == 0 && got.Role != RoleMaintainer) || (i == 4 && got.Enabled) {
			t.Fatalf("成员统计错误：%+v，期望 %+v", got, want)
		}
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到权限页查询")
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
	loops := 0
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "admin_sessions" {
			loops += n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if loops != 5 {
		t.Fatalf("成员会话读取执行了 %d 次，期望每位所有者及普通成员一次，共 5 次", loops)
	}
}
