package account

import (
	"errors"
	"reflect"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBannedOwnerGuardQueryReusesSQL(t *testing.T) {
	for _, name := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "guard-query-owner@example.test"})
			blocked := complete(t, db, Identity{"email", "guard-query-blocked@example.test"})
			if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now() WHERE id=$1`, blocked.User.ID); err != nil {
				t.Fatal(err)
			}
			section := moderationSections[name]
			ids := []string{"fc334455-1234-4234-8234-123456789ab1", "fc334455-1234-4234-8234-123456789ab2"}
			for i, user := range []string{owner.User.ID, blocked.User.ID} {
				switch name {
				case "skins":
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'合成作品','{}')`, ids[i], user); err != nil {
						t.Fatal(err)
					}
				case "candidate-skins":
					insertCandidateSkin(t, db, ids[i], user, "合成作品")
				case "plugins":
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'sound','query-test','合成作品','1.0','MIT','合成清单','合成归档',repeat('a',64))`, ids[i], user); err != nil {
						t.Fatal(err)
					}
				default:
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,$3,'合成作品','{}')`, ids[i], user, section.kind); err != nil {
						t.Fatal(err)
					}
				}
			}
			if section.kind != "" {
				otherKind := "dictionary"
				if section.kind == otherKind {
					otherKind = "reply"
				}
				if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES('guard-other-kind',$1,$2,'其他类型作品','{}')`, blocked.User.ID, otherKind); err != nil {
					t.Fatal(err)
				}
			}
			trace := &communityPaginationTrace{table: "auth_users", queries: make(chan communityPageQuery, 4)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			requests := [][]string{{ids[0]}, {ids[0]}, {ids[1]}, {ids[0]}}
			if section.kind != "" {
				requests = append(requests, []string{"guard-other-kind"})
			}
			var firstSQL string
			for request, selected := range requests {
				if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=CASE WHEN $2 THEN now() END WHERE id=$1`, owner.User.ID, request == 1); err != nil {
					t.Fatal(err)
				}
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				err = bannedOwnerGuard(t.Context(), tx, section, selected)
				if rollbackErr := tx.Rollback(t.Context()); rollbackErr != nil {
					t.Fatal(rollbackErr)
				}
				if request == 1 || request == 2 {
					var failure *actionError
					if !errors.As(err, &failure) || failure.status != 409 || failure.code != "owner_banned" {
						t.Fatal("未拒绝当前已封禁的作者", err)
					}
				} else if err != nil {
					t.Fatal("未按当前分区和封禁状态判断", err)
				}
				if len(trace.queries) != 1 {
					t.Fatalf("记录到 %d 条作者检查查询，期望 1 条", len(trace.queries))
				}
				query := <-trace.queries
				if len(query.args) != 1 || !reflect.DeepEqual(query.args[0], selected) {
					t.Fatal("作者检查的作品参数变化", query.args)
				}
				if firstSQL == "" {
					firstSQL = query.sql
					continue
				}
				if query.sql != firstSQL {
					t.Fatal("作者检查 SQL 随作品或封禁状态变化")
				}
				// 保留实际执行的语句，检出重复构建固定作者检查 SQL 的回归。
				if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL) {
					t.Error("作者检查逐请求分配 SQL，期望复用初始化结果")
				}
			}
		})
	}
}
