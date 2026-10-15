package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommunityStatsScansResourcesOnce(t *testing.T) {
	db := testStore(t)
	var raw json.RawMessage
	if err := db.pool.QueryRow(t.Context(), `EXPLAIN (FORMAT JSON) `+communityStatsQuery).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Relation string `json:"Relation Name"`
		Plans    []node `json:"Plans"`
	}
	var plans []struct {
		Plan node `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatal("无法读取统计查询计划：", err, string(raw))
	}
	// 检查实际数据库计划中的资源表扫描分支，避免计数随资源种类增加而重复扫表。
	scans := 0
	var visit func(node)
	visit = func(n node) {
		if n.Relation == "community_resources" {
			scans++
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	if scans != 1 {
		t.Fatalf("资源表扫描分支数应为 1，实际为 %d", scans)
	}
}

func TestCommunityResourceStatsKindsAndModeration(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	mux := http.NewServeMux()
	Mount(mux, a)
	owner := complete(t, db, Identity{"email", "resource-stats@example.test"})
	reader := complete(t, db, Identity{"email", "resource-stats-reader@example.test"})
	for _, kind := range []string{"dictionary", "reply", "phrase"} {
		for _, state := range []string{"approved", "pending", "removed"} {
			id := "stats-" + kind + "-" + state
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES($1,$2,$3,$1,'{}',$4)`, id, owner.User.ID, kind, state); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resource_saves(resource_id,user_id) VALUES($1,$2)`, id, reader.User.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func(t *testing.T, dictionaries, replies, saves int64) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/community/stats", nil))
		var stats CommunityStats
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stats) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		r := httptest.NewRequest("GET", "/api/overview", nil)
		r = r.WithContext(adminTestContext(r.Context(), "legacy-token"))
		w = httptest.NewRecorder()
		a.AdminHTTP(w, r)
		var counts struct {
			Dictionaries  int64 `json:"dictionaries"`
			Replies       int64 `json:"replies"`
			ResourceSaves int64 `json:"resource_saves"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &counts) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		// 两个接口都包括待审核词库与回复模板及其收藏，排除短语包和已移除资源。
		if stats.Dictionaries != dictionaries || stats.Replies != replies || stats.ResourceSaves != saves || counts.Dictionaries != dictionaries || counts.Replies != replies || counts.ResourceSaves != saves {
			t.Fatalf("公共统计与后台计数口径错误，期望词库=%d 回复=%d 收藏=%d：公共=%+v 后台=%+v", dictionaries, replies, saves, stats, counts)
		}
	}
	t.Run("仅统计未下架词库与模板的收藏", func(t *testing.T) { check(t, 2, 2, 4) })
	if _, err := db.pool.Exec(t.Context(), `UPDATE community_resources SET moderation='approved' WHERE id='stats-dictionary-removed'`); err != nil {
		t.Fatal(err)
	}
	t.Run("恢复作品后计入保留的收藏", func(t *testing.T) { check(t, 3, 2, 5) })
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resource_saves(resource_id,user_id) VALUES('stats-dictionary-approved',$1)`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	t.Run("同一作品的不同账号分别计数", func(t *testing.T) { check(t, 3, 2, 6) })
	if _, err := db.pool.Exec(t.Context(), `DELETE FROM community_resource_saves WHERE resource_id='stats-reply-pending'`); err != nil {
		t.Fatal(err)
	}
	t.Run("取消收藏后减少计数", func(t *testing.T) { check(t, 3, 2, 5) })
}
