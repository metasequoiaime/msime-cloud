package account

import (
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminContentDeleteQueryReusesSQL(t *testing.T) {
	for _, tc := range []struct{ section, action string }{
		{"skins", "delete_skin"}, {"candidate-skins", "delete_candidate_skin"}, {"plugins", "delete_plugin"},
	} {
		t.Run(tc.section, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			owner := complete(t, db, Identity{"email", "delete-query-owner@example.test"})
			ids := []string{"dc334455-1234-4234-8234-123456789ab1", "dc334455-1234-4234-8234-123456789ab2", "dc334455-1234-4234-8234-123456789ab3"}
			tokens := make([]string, len(ids))
			for i, id := range ids {
				switch tc.section {
				case "skins":
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'合成作品','{}')`, id, owner.User.ID); err != nil {
						t.Fatal(err)
					}
				case "candidate-skins":
					insertCandidateSkin(t, db, id, owner.User.ID, "合成作品")
				case "plugins":
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'sound','query-test','合成作品','1.0','MIT','合成清单','合成归档',repeat('a',64))`, id, owner.User.ID); err != nil {
						t.Fatal(err)
					}
				}
				token, ok := adminEditDetail(t, a, tc.section, id)["edit_token"].(string)
				if !ok || len(token) != 32 {
					t.Fatal("没有有效编辑凭据")
				}
				tokens[i] = token
			}
			table := moderationSections[tc.section].table
			trace := &communityPaginationTrace{table: table, queries: make(chan communityPageQuery, 4)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a = &Service{store: &Store{pool: pool}}
			var firstSQL string
			for request, id := range []string{ids[0], ids[1], ids[2], ids[1]} {
				index := min(request, 1)
				var value any = map[string]any{"edit_token": tokens[index]}
				if request == 2 {
					value = nil
				}
				want := 200
				if request == 3 {
					want = 409
				}
				if w := contentEditCall(t, a, tc.action, tc.section, id, value); w.Code != want {
					t.Fatal("删除状态错误", w.Code, w.Body.String())
				}
				if len(trace.queries) != 1 {
					t.Fatalf("记录到 %d 条删除查询，期望 1 条", len(trace.queries))
				}
				query := <-trace.queries
				if request == 2 {
					if query.sql != `DELETE FROM `+table+` WHERE id=$1` || len(query.args) != 1 || query.args[0] != id {
						t.Fatal("无凭据删除语义变化", query)
					}
					continue
				}
				if len(query.args) != 2 || query.args[0] != id || query.args[1] != tokens[index] {
					t.Fatal("版本化删除参数变化", query.args)
				}
				if firstSQL == "" {
					firstSQL = query.sql
					continue
				}
				if query.sql != firstSQL {
					t.Fatal("版本化删除查询随请求变化")
				}
				// 保留实际执行的语句，检出按请求拼接相同版本条件的回归。
				if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
					t.Error("版本化删除逐请求分配 SQL，期望复用动作初始化结果")
				}
			}
		})
	}
}
