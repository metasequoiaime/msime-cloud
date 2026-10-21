package account

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminModerationUndoTrace struct {
	table   string
	queries chan communityPageQuery
}

func (trace *adminModerationUndoTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT "+moderationVersionSQL+",moderation,") ||
		strings.HasPrefix(data.SQL, "SELECT u.id,u.banned_at IS NOT NULL FROM auth_users u WHERE u.id=(SELECT owner_id FROM "+trace.table) ||
		strings.HasPrefix(data.SQL, "SELECT id FROM "+trace.table+" WHERE id=$1 AND owner_id=$2") ||
		strings.HasPrefix(data.SQL, "UPDATE "+trace.table+" SET moderation=$2,previous_moderation=$3") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}
func (*adminModerationUndoTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestModerationUndoQueriesReuseSQL(t *testing.T) {
	for _, name := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_audit`); err != nil {
				t.Fatal(err)
			}
			owner := complete(t, db, Identity{"email", "undo-query-owner@example.test"})
			section := moderationSections[name]
			ids := []string{"dc334455-1234-4234-8234-123456789ab1", "dc334455-1234-4234-8234-123456789ab2"}
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

			if _, err := db.pool.Exec(t.Context(), `UPDATE `+section.table+` SET moderation='pending',previous_moderation=NULL,moderation_reason=NULL WHERE id=ANY($1)`, ids); err != nil {
				t.Fatal(err)
			}
			trace := &adminModerationUndoTrace{table: section.table, queries: make(chan communityPageQuery, 16)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a := &Service{store: &Store{pool: pool}}
			firstSQL := map[string]string{}
			for _, id := range ids {
				undo := moderationUndoBody(t, contentEditCall(t, a, "approve_content", name, id, nil))
				for step := range 3 {
					if step > 0 {
						w := httptest.NewRecorder()
						a.AdminHTTP(w, adminJSONRequest("POST", "/api/actions", undo))
						want := 200
						if step == 2 {
							want = 409
						}
						if w.Code != want {
							t.Fatal("撤销状态错误", w.Code, w.Body.String())
						}
					}
					wantState, count := "approved", 4
					if step > 0 {
						wantState = "pending"
					}
					if step == 2 {
						count = 3
					}
					state, previous, reason, actor := moderationState(t, db, section.table, id)
					if state != wantState || previous != nil || reason != nil || actor == nil || *actor != "legacy-token" {
						t.Fatal("撤销持久化状态错误", state, previous, reason, actor)
					}
					if len(trace.queries) != count {
						t.Fatalf("记录到 %d 条撤销查询，期望 %d 条", len(trace.queries), count)
					}
					for range count {
						q := <-trace.queries
						kind, args := "snapshot", []any{id}
						if strings.HasPrefix(q.sql, "SELECT u.id,") {
							kind = "owner-lock"
						} else if strings.HasPrefix(q.sql, "SELECT id FROM ") {
							kind, args = "item-lock", []any{id, owner.User.ID}
						} else if strings.HasPrefix(q.sql, "UPDATE ") {
							kind, args = "update", []any{id, "pending", (*string)(nil), (*string)(nil), "legacy-token"}
						}
						if !reflect.DeepEqual(q.args, args) {
							t.Fatalf("%s 参数错误：%v，期望 %v", kind, q.args, args)
						}
						if firstSQL[kind] == "" {
							firstSQL[kind] = q.sql
							continue
						}
						if q.sql != firstSQL[kind] {
							t.Fatal("撤销查询随作品或版本变化", kind)
						}
						// 保留实际执行的语句，检出快照和行锁 SQL 被逐请求构建的回归。
						if unsafe.StringData(q.sql) != unsafe.StringData(firstSQL[kind]) {
							t.Errorf("%s 逐请求分配 SQL，期望复用初始化结果", kind)
						}
					}
				}
			}
			if len(firstSQL) != 4 {
				t.Fatal("未覆盖全部撤销查询", firstSQL)
			}
			var audits int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_audit WHERE action='undo_moderation'`).Scan(&audits); err != nil || audits != 2 {
				t.Fatal("失败撤销产生审计记录", audits, err)
			}
		})
	}
}
