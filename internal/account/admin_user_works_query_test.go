package account

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminUserWorksStatisticsRunAfterLimit(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "admin-works-owner@example.test"})
	other := complete(t, db, Identity{"email", "admin-works-other@example.test"})
	for _, query := range []string{
		`INSERT INTO community_skins(id,owner_id,name,design,created_at) SELECT 'works-keyboard-'||n,$1,'键盘','{}',now()-n*interval '1 minute' FROM generate_series(1,30) n`,
		`INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,created_at,visibility) SELECT md5('works-package-'||n)::uuid::text,$1,'works-candidate-'||n,'候选窗','1','MIT','x','preview.png',repeat('a',64),now()-n*interval '1 minute',CASE WHEN n%2=0 THEN 'private' ELSE 'public' END FROM generate_series(1,20) n`,
		`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,created_at) SELECT md5('works-package-'||n)::uuid::text,$1,'sound','works-plugin-'||n,'插件','1','MIT','x','x',repeat('b',64),now()-n*interval '1 minute' FROM generate_series(1,20) n`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content,created_at,moderation,moderation_reason) SELECT 'works-resource-'||n,$1,CASE n%3 WHEN 0 THEN 'dictionary' WHEN 1 THEN 'reply' ELSE 'phrase' END,'资源','{}',now()-n*interval '1 minute',CASE WHEN n%2=0 THEN 'removed' ELSE 'pending' END,'合成审核原因' FROM generate_series(1,30) n`,
		`INSERT INTO community_skin_downloads SELECT id,$2 FROM community_skins WHERE owner_id=$1`,
		`INSERT INTO community_candidate_skin_downloads SELECT id,$2 FROM community_candidate_skins WHERE owner_id=$1`,
		`INSERT INTO community_plugin_downloads SELECT id,$2 FROM community_plugins WHERE owner_id=$1`,
		`INSERT INTO community_plugin_downloads SELECT id,$1 FROM community_plugins WHERE owner_id=$1`,
		`INSERT INTO community_resource_saves(resource_id,user_id) SELECT id,$2 FROM community_resources WHERE owner_id=$1`,
	} {
		args := []any{owner.User.ID}
		if strings.Contains(query, "$2") {
			args = append(args, other.User.ID)
		}
		if _, err := db.pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_skins(id,owner_id,name,design) VALUES('works-other',$1,'其他账号','{}')`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	var expected []struct {
		ID      string `json:"id"`
		Section string `json:"section"`
	}
	rows, err := db.pool.Query(t.Context(), `SELECT id,section FROM (
 SELECT id,'skins' AS section,created_at FROM community_skins WHERE owner_id=$1
 UNION ALL SELECT id,'candidate-skins',created_at FROM community_candidate_skins WHERE owner_id=$1
 UNION ALL SELECT id,'plugins',created_at FROM community_plugins WHERE owner_id=$1
 UNION ALL SELECT id,CASE kind WHEN 'dictionary' THEN 'dictionaries' WHEN 'reply' THEN 'replies' ELSE 'phrases' END,created_at FROM community_resources WHERE owner_id=$1
 ) works ORDER BY created_at DESC,id DESC LIMIT 20`, owner.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var item struct {
			ID      string `json:"id"`
			Section string `json:"section"`
		}
		if err := rows.Scan(&item.ID, &item.Section); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	trace := &communityPaginationTrace{table: "auth_users u", queries: make(chan communityPageQuery, 4)}
	cfg := db.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := &Service{store: &Store{pool: pool}}
	w := httptest.NewRecorder()
	a.adminUser(w, httptest.NewRequest("GET", "/api/users/"+owner.User.ID, nil), owner.User.ID)
	var detail struct {
		Works []struct {
			ID         string `json:"id"`
			Section    string `json:"section"`
			Downloads  int    `json:"downloads"`
			Saves      int    `json:"saves"`
			Moderation string `json:"moderation"`
			Reason     string `json:"moderation_reason"`
		} `json:"works"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || len(detail.Works) != 20 {
		t.Fatal(w.Code, w.Body.String())
	}
	seen := map[string]bool{}
	for i, item := range detail.Works {
		if item.ID != expected[i].ID || item.Section != expected[i].Section {
			t.Fatalf("最近作品或顺序错误：%+v，期望 %+v", item, expected[i])
		}
		seen[item.Section] = true
		if item.Section == "skins" || item.Section == "candidate-skins" || item.Section == "plugins" {
			downloads := 1
			if item.Section == "plugins" {
				downloads = 2
			}
			if item.Downloads != downloads || item.Saves != 0 {
				t.Fatalf("下载统计错误：%+v", item)
			}
		} else if item.Downloads != 0 || item.Saves != 1 || item.Reason != "合成审核原因" || (item.Moderation != "removed" && item.Moderation != "pending") {
			t.Fatalf("收藏或审核字段错误：%+v", item)
		}
	}
	for _, section := range []string{"skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"} {
		if !seen[section] {
			t.Fatalf("测试未覆盖 %s 分区", section)
		}
	}
	var query communityPageQuery
	select {
	case query = <-trace.queries:
	default:
		t.Fatal("没有记录到用户详情查询")
	}
	var raw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (ANALYZE,TIMING OFF,FORMAT JSON) `+query.sql, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Loops    int    `json:"Actual Loops"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal(err, string(raw))
	}
	loops := 0
	var visit func(node)
	visit = func(n node) {
		switch n.Relation {
		case "community_skin_downloads", "community_candidate_skin_downloads", "community_plugin_downloads", "community_resource_saves":
			loops += n.Loops
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if loops != 20 {
		t.Errorf("作品互动统计执行了 %d 次，期望只统计最近 20 件作品", loops)
	}
	empty := complete(t, db, Identity{"email", "admin-works-empty@example.test"})
	emptyResponse := httptest.NewRecorder()
	a.adminUser(emptyResponse, httptest.NewRequest("GET", "/api/users/"+empty.User.ID, nil), empty.User.ID)
	var emptyDetail struct {
		Works json.RawMessage `json:"works"`
	}
	if emptyResponse.Code != 200 || json.Unmarshal(emptyResponse.Body.Bytes(), &emptyDetail) != nil || string(emptyDetail.Works) != "[]" {
		t.Fatal("零作品应返回空数组：", emptyResponse.Code, emptyResponse.Body.String())
	}
}
