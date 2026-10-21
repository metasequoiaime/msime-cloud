package account

import (
	"reflect"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSkinCategoryQueryReusesSQL(t *testing.T) {
	for _, section := range []string{"skins", "candidate-skins"} {
		t.Run(section, func(t *testing.T) {
			db, a := usersTestStore(t)
			id := seedCategoryUndo(t, db, section)
			table := moderationSections[section].table
			trace := &communityPaginationTrace{table: table, queries: make(chan communityPageQuery, 4)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a.store = &Store{pool: pool}
			action := "set_skin_category"
			if section == "candidate-skins" {
				action = "set_candidate_skin_category"
			}
			var firstSQL string
			var undo statusUndoRequest
			for request, category := range []string{"cute", "tech", "cute"} {
				var input any = map[string]any{"action": action, "id": id, "value": map[string]any{"category": category}}
				if request == 1 {
					input = map[string]any{"action": action, "id": id, "value": map[string]any{"category": category, "expected": "cute"}}
				} else if request == 2 {
					input = undo
				}
				undo = statusUndoFrom(t, statusUndoCall(t, a, input))
				var actual string
				if err := db.pool.QueryRow(t.Context(), `SELECT category FROM `+table+` WHERE id=$1`, id).Scan(&actual); err != nil || actual != category {
					t.Fatal("分类未按请求更新", actual, err)
				}
				if len(trace.queries) != 1 {
					t.Fatalf("记录到 %d 条分类更新查询，期望 1 条", len(trace.queries))
				}
				query := <-trace.queries
				if len(query.args) != 4 || !reflect.DeepEqual(query.args[0], []string{id}) || query.args[1] != category {
					t.Fatal("分类或作品参数发生变化", query.args)
				}
				expected, ok := query.args[2].(*string)
				if !ok || (request == 1 && (expected == nil || *expected != "cute")) || (request != 1 && expected != nil) {
					t.Fatal("前态条件未按请求绑定", query.args[2])
				}
				token, ok := query.args[3].(*string)
				if !ok || (request == 2 && (token == nil || len(*token) != 32)) || (request != 2 && token != nil) {
					t.Fatal("撤销版本未按请求绑定")
				}
				if firstSQL == "" {
					firstSQL = query.sql
					continue
				}
				if query.sql != firstSQL {
					t.Fatal("分类更新 SQL 随请求变化")
				}
				// 比较实际执行语句的只读数据，检出重复构建固定 SQL 的回归。
				if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
					t.Error("分类更新逐请求分配 SQL，期望复用动作初始化结果")
				}
			}
		})
	}
}
