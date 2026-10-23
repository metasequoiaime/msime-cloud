package account

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminApprovalTrace struct {
	table   string
	queries chan communityPageQuery
}

func (trace *adminApprovalTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "UPDATE "+trace.table+" SET moderation='approved'") || strings.HasPrefix(data.SQL, "SELECT EXISTS(SELECT 1 FROM "+trace.table+" WHERE id=ANY($1)") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*adminApprovalTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestApprovalQueriesReuseSQL(t *testing.T) {
	for _, name := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "approval-query-owner@example.test"})
			section := moderationSections[name]
			ids := []string{"ac334455-1234-4234-8234-123456789ab1", "ac334455-1234-4234-8234-123456789ab2"}
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
			trace := &adminApprovalTrace{table: section.table, queries: make(chan communityPageQuery, 8)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a, reader := &Service{store: &Store{pool: pool}}, &Service{store: db}
			firstSQL := map[string]string{}
			for _, mode := range []string{"stale", "plain", "token"} {
				if mode == "token" && name != "skins" && name != "candidate-skins" && name != "plugins" {
					continue
				}
				if _, err := db.pool.Exec(t.Context(), `UPDATE `+section.table+` SET moderation='pending',moderation_reason=NULL WHERE id=ANY($1)`, ids); err != nil {
					t.Fatal(err)
				}
				for i, id := range ids {
					var value any
					var from *string
					var token string
					if mode != "plain" || i != 0 {
						state := "pending"
						if mode == "stale" {
							state = "removed"
						}
						from = &state
						input := map[string]any{"from": state}
						if mode == "token" {
							token = adminEditDetail(t, reader, name, id)["edit_token"].(string)
							input["edit_token"] = token
						}
						value = input
					}
					want, state, count := 200, "approved", 1
					if mode == "stale" {
						want, state, count = 409, "pending", 2
					}
					if w := contentEditCall(t, a, "approve_content", name, id, value); w.Code != want {
						t.Fatal("审核状态错误", w.Code, w.Body.String())
					}
					if actual, _, _, _ := moderationState(t, db, section.table, id); actual != state {
						t.Fatal("审核持久化状态错误", actual, state)
					}
					if len(trace.queries) != count {
						t.Fatalf("记录到 %d 条审核查询，期望 %d 条", len(trace.queries), count)
					}
					for range count {
						query := <-trace.queries
						kind := "update"
						args := []any{[]string{id}, "legacy-token", from, (*time.Time)(nil)}
						if section.editable {
							args = append(args, (*time.Time)(nil))
						}
						if token != "" {
							kind, args = "token-update", append(args, token)
						}
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
							t.Fatalf("%s SQL 随请求变化", kind)
						}
						// 保留实际执行的语句，检出审核动作重复构建固定 SQL 的回归。
						if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL[kind]) {
							t.Errorf("%s 逐请求分配 SQL，期望复用初始化结果", kind)
						}
					}
				}
			}
		})
	}
}
