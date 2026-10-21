package account

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminModerationStateTrace struct {
	table   string
	queries chan communityPageQuery
}

func (trace *adminModerationStateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "UPDATE "+trace.table+" SET ") || strings.HasPrefix(data.SQL, "SELECT EXISTS(SELECT 1 FROM "+trace.table+" WHERE id=ANY($1)") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}
func (*adminModerationStateTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestModerationStateQueriesReuseSQL(t *testing.T) {
	for _, name := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "state-query-owner@example.test"})
			section := moderationSections[name]
			ids := []string{"bc334455-1234-4234-8234-123456789ab1", "bc334455-1234-4234-8234-123456789ab2"}
			for _, id := range ids {
				switch name {
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
				default:
					if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,$3,'合成作品','{}')`, id, owner.User.ID, section.kind); err != nil {
						t.Fatal(err)
					}
				}
			}

			if _, err := db.pool.Exec(t.Context(), `UPDATE `+section.table+` SET moderation='pending' WHERE id=ANY($1)`, ids); err != nil {
				t.Fatal(err)
			}
			trace := &adminModerationStateTrace{table: section.table, queries: make(chan communityPageQuery, 8)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a := &Service{store: &Store{pool: pool}}
			ctx := WithAdminAccess(t.Context(), AdminAccess{Actor: "query-admin"})
			firstSQL := map[string]string{}
			for _, id := range ids {
				previous := "approved"
				for _, step := range []struct {
					action            adminActionFunc
					value             string
					kind, state, code string
					args              []any
				}{
					{actionRemoveContent, "", "remove", "removed", "", []any{[]string{id}, "query-admin", "合成下架原因", (*string)(nil)}},
					{actionRemoveContent, `{"previous":"approved"}`, "remove", "removed", "", []any{[]string{id}, "query-admin", "合成下架原因", &previous}},
					{actionRestoreContent, "", "restore", "pending", "", []any{[]string{id}, "query-admin"}},
					{actionRestoreContent, "", "restore", "pending", "not_removed", []any{[]string{id}, "query-admin"}},
					{actionRestoreContent, `{"to":"approved"}`, "undo", "approved", "", []any{[]string{id}, "query-admin", "approved"}},
					{actionRestoreContent, `{"to":"pending"}`, "undo", "pending", "", []any{[]string{id}, "query-admin", "pending"}},
				} {
					tx, err := pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = step.action(a, ctx, tx, actionRequest{Section: name, ID: id, Reason: "合成下架原因", Value: json.RawMessage(step.value)})
					if step.code == "" {
						if err != nil {
							tx.Rollback(ctx)
							t.Fatal(err)
						}
						if err = tx.Commit(ctx); err != nil {
							t.Fatal(err)
						}
					} else {
						if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
							t.Fatal(rollbackErr)
						}
						var failure *actionError
						if !errors.As(err, &failure) || failure.status != 409 || failure.code != step.code {
							t.Fatal("错误返回变化", err)
						}
					}
					state, savedPrevious, reason, actor := moderationState(t, db, section.table, id)
					if state != step.state || actor == nil || *actor != "query-admin" {
						t.Fatal("审核状态或操作者错误", state, actor)
					}
					if step.kind == "remove" {
						if savedPrevious == nil || *savedPrevious != "pending" || reason == nil || *reason != "合成下架原因" {
							t.Fatal("重复下架丢失原状态", savedPrevious, reason)
						}
					} else if savedPrevious != nil || reason != nil {
						t.Fatal("恢复残留下架状态", savedPrevious, reason)
					}
					count := 1
					if step.code != "" {
						count++
					}
					if len(trace.queries) != count {
						t.Fatalf("记录到 %d 条查询，期望 %d 条", len(trace.queries), count)
					}
					for range count {
						query := <-trace.queries
						kind, args := step.kind, step.args
						if strings.HasPrefix(query.sql, "SELECT EXISTS(") {
							kind, args = "exists", []any{[]string{id}}
						}
						if !reflect.DeepEqual(query.args, args) {
							t.Fatalf("%s 参数错误：%v，期望 %v", kind, query.args, args)
						}
						if firstSQL[kind] == "" {
							firstSQL[kind] = query.sql
							continue
						}
						if query.sql != firstSQL[kind] {
							t.Fatal("审核 SQL 随请求变化", kind)
						}
						// 保留执行过的语句，检出固定状态变更 SQL 被逐请求重新分配的回归。
						if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL[kind]) {
							t.Errorf("%s 逐请求分配 SQL，期望复用初始化结果", kind)
						}
					}
				}
			}
		})
	}
}
