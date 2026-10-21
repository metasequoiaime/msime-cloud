package account

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type adminContentSaveTrace struct {
	table   string
	queries chan communityPageQuery
}

func (trace *adminContentSaveTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if (strings.HasPrefix(data.SQL, "SELECT owner_id,md5(") && strings.Contains(data.SQL, " FROM "+trace.table+" ")) || strings.HasPrefix(data.SQL, "UPDATE "+trace.table+" SET moderation=$2") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}

func (*adminContentSaveTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAdminContentSaveQueriesReuseSQL(t *testing.T) {
	for _, tc := range []struct{ section, kind string }{{"skins", "skin"}, {"candidate-skins", "candidate_skin"}, {"plugins", "plugin"}} {
		t.Run(tc.section, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "save-query-owner@example.test"})
			trace := &adminContentSaveTrace{table: moderationSections[tc.section].table, queries: make(chan communityPageQuery, 4)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a, reader := &Service{store: &Store{pool: pool}}, &Service{store: db}
			firstSQL := map[string]string{}
			for _, create := range []bool{true, false} {
				for i, id := range []string{"ec334455-1234-4234-8234-123456789ab1", "ec334455-1234-4234-8234-123456789ab2"} {
					name, state, action := []string{"合成作品一", "合成作品二"}[i], []string{"pending", "approved"}[i], "create_"+tc.kind
					input := map[string]any{"owner_id": owner.User.ID, "name": name, "moderation": state}
					switch tc.section {
					case "skins":
						input["design"], input["category"] = json.RawMessage(communityFixture), "nature"
					case "candidate-skins":
						manifest, files := candidateFixture(t, "save-query")
						input["manifest"], input["files"], input["category"], input["visibility"] = manifest, files, "nature", "public"
					case "plugins":
						input["archive"] = pluginSoundZip(t)
					}
					var previousToken any
					if !create {
						previousToken = adminEditDetail(t, reader, tc.section, id)["edit_token"]
						name, state, action = name+"修改", []string{"removed", "pending"}[i], "update_"+tc.kind
						input["name"], input["moderation"], input["edit_token"] = name, state, previousToken
					}
					if w := contentEditCall(t, a, action, tc.section, id, input); w.Code != 200 {
						t.Fatal("保存内容失败", w.Code, w.Body.String())
					}
					detail := adminEditDetail(t, reader, tc.section, id)
					if detail["name"] != name || detail["moderation"] != state || detail["owner_id"] != owner.User.ID || (!create && detail["edit_token"] == previousToken) {
						t.Fatal("保存后的作品或版本错误", detail)
					}
					count := 2
					if create {
						count = 1
					}
					if len(trace.queries) != count {
						t.Fatalf("记录到 %d 条保存查询，期望 %d 条", len(trace.queries), count)
					}
					for range count {
						query := <-trace.queries
						kind := "edit-moderation"
						args := []any{id, state, "legacy-token", create}
						if strings.HasPrefix(query.sql, "SELECT owner_id,") {
							kind, args = "edit-lock", []any{id}
						} else if create {
							kind = "create-moderation"
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
						// 保留实际语句，检出保存动作逐请求构建固定 SQL 的回归。
						if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL[kind]) {
							t.Errorf("%s 逐请求分配 SQL，期望复用动作初始化结果", kind)
						}
					}
				}
			}
		})
	}
}
