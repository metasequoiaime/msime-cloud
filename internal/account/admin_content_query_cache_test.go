package account

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminContentQueriesReuseSQL(t *testing.T) {
	for _, section := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		t.Run(section, func(t *testing.T) {
			db := testStore(t)
			owner := complete(t, db, Identity{"email", "content-query-owner@example.test"})
			table := moderationSections[section].table
			ids := []string{"cc334455-1234-4234-8234-123456789ab1", "cc334455-1234-4234-8234-123456789ab2"}
			for _, id := range ids {
				var query string
				args := []any{id, owner.User.ID}
				switch section {
				case "skins":
					query = `INSERT INTO community_skins(id,owner_id,name,design) VALUES($1,$2,'合成作品','{}')`
				case "candidate-skins":
					query = `INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256) VALUES($1,$2,'query-test','合成作品','1.0','MIT','合成清单','preview.png',repeat('a',64))`
				case "plugins":
					query = `INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256) VALUES($1,$2,'sound','query-test','合成作品','1.0','MIT','合成清单','合成归档',repeat('a',64))`
				default:
					query = `INSERT INTO community_resources(id,owner_id,kind,name,content) VALUES($1,$2,$3,'合成作品','{}')`
					args = append(args, moderationSections[section].kind)
				}
				if _, err := db.pool.Exec(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			trace := &communityPaginationTrace{table: table, queries: make(chan communityPageQuery, 8)}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			a := &Service{store: &Store{pool: pool}}
			firstSQL := map[string]string{}
			var previousToken string
			for request, id := range []string{ids[0], ids[1], ids[1]} {
				name := "合成作品"
				if request == 2 {
					name = "修改后的合成作品"
					if _, err := db.pool.Exec(t.Context(), `UPDATE `+table+` SET name=$2 WHERE id=$1`, id, name); err != nil {
						t.Fatal(err)
					}
				}
				w := httptest.NewRecorder()
				a.adminContent(w, httptest.NewRequest("GET", "/api/"+section+"/"+id, nil), section, id)
				var detail struct {
					ID, Name   string
					EditToken  string      `json:"edit_token"`
					OwnerItems []ownerItem `json:"owner_items"`
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.ID != id || detail.Name != name {
					t.Fatal("详情未读取当前作品", w.Code, w.Body.String())
				}
				otherID := ids[0]
				if id == ids[0] {
					otherID = ids[1]
				}
				if len(detail.OwnerItems) != 1 || detail.OwnerItems[0].ID != otherID || detail.OwnerItems[0].Section != section {
					t.Fatal("作者其他作品未排除当前作品", detail.OwnerItems)
				}
				if section == "skins" || section == "candidate-skins" || section == "plugins" {
					if detail.EditToken == "" || (request == 2 && detail.EditToken == previousToken) {
						t.Fatal("编辑凭据未随作品变化")
					}
					previousToken = detail.EditToken
				}
				if len(trace.queries) != 2 {
					t.Fatalf("记录到 %d 条详情查询，期望基础详情和作者作品各一条", len(trace.queries))
				}
				for range 2 {
					query := <-trace.queries
					kind := "detail"
					args := []any{id}
					if strings.HasPrefix(query.sql, "SELECT section,id,name,") {
						kind, args = "owner_items", []any{owner.User.ID, section, id}
					} else if moderationSections[section].kind != "" {
						args = append(args, moderationSections[section].kind)
					}
					if !reflect.DeepEqual(query.args, args) {
						t.Fatalf("%s 查询参数错误：%v，期望 %v", kind, query.args, args)
					}
					if firstSQL[kind] == "" {
						firstSQL[kind] = query.sql
						continue
					}
					if query.sql != firstSQL[kind] {
						t.Fatalf("%s 固定查询随请求变化", kind)
					}
					// 保留实际执行的语句，检出重复构建相同 SQL 字符串的回归。
					if unsafe.StringData(query.sql) != unsafe.StringData(firstSQL[kind]) {
						t.Errorf("%s 查询逐请求分配 SQL，期望复用初始化结果", kind)
					}
				}
			}
		})
	}
}
