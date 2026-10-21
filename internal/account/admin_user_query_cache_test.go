package account

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminUserQueryReusesSQL(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "query-cache-owner@example.test"})
	other := complete(t, db, Identity{"email", "query-cache-other@example.test"})
	trace := &communityPaginationTrace{table: "auth_users u", queries: make(chan communityPageQuery, 4)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var firstSQL string
	for _, tc := range []struct {
		id     string
		owners []string
		role   string
	}{
		{owner.User.ID, []string{"query-cache-owner@example.test"}, "maintainer"},
		{owner.User.ID, nil, "user"},
		{other.User.ID, []string{"query-cache-owner@example.test"}, "user"},
	} {
		a := &Service{store: &Store{pool: pool}, admin: AdminSettings{Owners: tc.owners}}
		w := httptest.NewRecorder()
		a.adminUser(w, httptest.NewRequest("GET", "/api/users/"+tc.id, nil), tc.id)
		var detail struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.ID != tc.id || detail.Role != tc.role {
			t.Fatalf("用户详情或所有者绑定错误：status=%d body=%s", w.Code, w.Body.String())
		}
		var query communityPageQuery
		select {
		case query = <-trace.queries:
		default:
			t.Fatal("没有记录到用户详情查询")
		}
		if len(query.args) != 2 || query.args[0] != tc.id {
			t.Fatalf("用户参数发生变化：%v", query.args)
		}
		owners, ok := query.args[1].([]string)
		if !ok || owners == nil || !slices.Equal(owners, tc.owners) {
			t.Fatalf("所有者参数发生变化：%v", query.args[1])
		}
		if firstSQL == "" {
			firstSQL = query.sql
			continue
		}
		if query.sql != firstSQL {
			t.Fatal("固定查询文本随请求变化")
		}
		// 同时保留两次查询，比较底层只读数据，直接检出逐请求创建的相同 SQL 字符串。
		if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
			t.Error("用户详情查询逐请求分配了 SQL 字符串，期望复用初始化结果")
		}
	}
}
