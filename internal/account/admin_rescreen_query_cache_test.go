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

type adminRescreenTrace struct {
	table   string
	queries chan communityPageQuery
}

func (trace *adminRescreenTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT id,json_build_object('name',name,'description',description,'content',") && strings.Contains(data.SQL, " FROM "+trace.table+" WHERE ") || strings.HasPrefix(data.SQL, "UPDATE "+trace.table+" AS target SET moderation_reason=v.reason") {
		trace.queries <- communityPageQuery{data.SQL, append([]any(nil), data.Args...)}
	}
	return ctx
}
func (*adminRescreenTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRescreenQueriesReuseSQL(t *testing.T) {
	for _, name := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "rescreen-query-owner@example.test"})
			section := moderationSections[name]
			ids := []string{"ec334455-1234-4234-8234-123456789ab1", "ec334455-1234-4234-8234-123456789ab2"}
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
			trace := &adminRescreenTrace{table: section.table, queries: make(chan communityPageQuery, 4)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a := &Service{store: &Store{pool: pool}}
			a.sensitive.loaded = time.Now().AddDate(100, 0, 0)
			firstSQL := map[string]string{}
			for request, id := range []string{ids[0], ids[0], ids[1]} {
				pattern := "合成作品"
				if request == 1 {
					pattern = "作品"
				}
				a.sensitive.words = []sensitiveWord{{hit: SensitiveHit{WordID: 1, Pattern: pattern, Level: SensitiveReview}, plain: pattern}}
				selected := []string{id}
				flag := "命中敏感词：「" + pattern + "」"
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err = a.rescreenRestored(t.Context(), tx, name, selected); err != nil {
					tx.Rollback(t.Context())
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, _, reason, _ := moderationState(t, db, section.table, id); reason == nil || *reason != flag {
					t.Fatal("未按当前规则重筛", reason, flag)
				}
				if len(trace.queries) != 2 {
					t.Fatalf("重筛用了 %d 条查询，期望读写各一次", len(trace.queries))
				}
				for range 2 {
					query := <-trace.queries
					kind, args := "select", []any{selected}
					if strings.HasPrefix(query.sql, "UPDATE ") {
						kind, args = "update", []any{selected, []string{flag}}
					}
					if !reflect.DeepEqual(query.args, args) {
						t.Fatalf("%s 参数错误：%v，期望 %v", kind, query.args, args)
					}
					if firstSQL[kind] == "" {
						firstSQL[kind] = query.sql
						continue
					}
					if query.sql != firstSQL[kind] {
						t.Fatal("重筛 SQL 随作品或规则变化", kind)
					}
					// 保留实际执行的语句，检出批量重筛 SQL 被逐请求重新构建的回归。
					if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL[kind]) {
						t.Errorf("%s 逐请求分配 SQL，期望复用初始化结果", kind)
					}
				}
			}
			if _, err := db.pool.Exec(t.Context(), `UPDATE `+section.table+` SET moderation='approved',moderation_reason='保留标记' WHERE id=$1`, ids[0]); err != nil {
				t.Fatal(err)
			}
			selected := []string{ids[0]}
			if section.kind != "" {
				otherKind := "dictionary"
				if section.kind == otherKind {
					otherKind = "reply"
				}
				if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation,moderation_reason) VALUES('rescreen-other-kind',$1,$2,'合成作品','{}','pending','保留标记')`, owner.User.ID, otherKind); err != nil {
					t.Fatal(err)
				}
				selected = append(selected, "rescreen-other-kind")
			}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err = a.rescreenRestored(t.Context(), tx, name, selected); err != nil {
				tx.Rollback(t.Context())
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(trace.queries) != 1 {
				t.Fatal("非待审或其他种类作品被重筛", len(trace.queries))
			}
			if q := <-trace.queries; q.sql != firstSQL["select"] || !reflect.DeepEqual(q.args, []any{selected}) {
				t.Fatal("空结果的读取语句或参数变化", q)
			}
			for _, id := range selected {
				if _, _, reason, _ := moderationState(t, db, section.table, id); reason == nil || *reason != "保留标记" {
					t.Fatal("改写了无关作品的标记", id, reason)
				}
			}
		})
	}
}
