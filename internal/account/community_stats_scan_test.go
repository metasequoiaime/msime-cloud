package account

import (
	"encoding/json"
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
	owner := complete(t, db, Identity{"email", "resource-stats@example.test"})
	for _, kind := range []string{"dictionary", "reply", "phrase"} {
		for _, state := range []string{"approved", "pending", "removed"} {
			id := "stats-" + kind + "-" + state
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO community_resources(id,owner_id,kind,name,content,moderation) VALUES($1,$2,$3,$1,'{}',$4)`, id, owner.User.ID, kind, state); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stats CommunityStats
	if err := db.pool.QueryRow(t.Context(), communityStatsQuery).Scan(&stats.Skins, &stats.SkinDownloads, &stats.Dictionaries, &stats.Replies, &stats.ResourceSaves, &stats.GeneratedAt); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/overview", nil)
	r = r.WithContext(adminTestContext(r.Context(), "legacy-token"))
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r)
	var counts struct {
		Dictionaries int64 `json:"dictionaries"`
		Replies      int64 `json:"replies"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &counts) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	// 两个接口都包括待审核资源，排除已移除资源；短语包不会混入词包或回复计数。
	if stats.Dictionaries != 2 || stats.Replies != 2 || counts.Dictionaries != stats.Dictionaries || counts.Replies != stats.Replies {
		t.Fatal("公共统计与后台计数口径错误：", stats, counts)
	}
}
