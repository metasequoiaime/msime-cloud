package account

import (
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestModerationCountsQueryReusesSQL(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "counts-query-owner@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES('counts-query-resource',$1,'dictionary','合成词库','{}','pending')`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	trace := &communityPaginationTrace{table: "community_skins", queries: make(chan communityPageQuery, 2)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	var firstSQL string
	for _, state := range []string{"pending", "approved"} {
		if _, err := db.pool.Exec(t.Context(), `UPDATE community_resources SET moderation=$1 WHERE id='counts-query-resource'`, state); err != nil {
			t.Fatal(err)
		}
		counts, err := a.moderationCounts(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		other := "pending"
		if state == "pending" {
			other = "approved"
		}
		if counts["dictionaries"][state] != 1 || counts["dictionaries"][other] != 0 {
			t.Fatal("审核计数未随数据库状态变化", counts)
		}
		var query communityPageQuery
		select {
		case query = <-trace.queries:
		default:
			t.Fatal("没有记录到审核统计查询")
		}
		if len(query.args) != 0 {
			t.Fatal("固定审核统计查询增加了参数", query.args)
		}
		if firstSQL == "" {
			firstSQL = query.sql
			continue
		}
		if query.sql != firstSQL {
			t.Fatal("固定审核统计查询随状态变化")
		}
		// 保留两次查询并比较只读数据，检出逐请求创建相同 SQL 字符串的回归。
		if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
			t.Error("审核统计查询逐请求分配 SQL，期望复用初始化结果")
		}
	}
}
