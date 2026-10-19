package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommunityMineListsUseOwnerIndex(t *testing.T) {
	db := testStore(t)
	owner := complete(t, db, Identity{"email", "catalog-owner@example.test"})
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id)
 SELECT 'catalog-user-'||n FROM generate_series(0,999) n`); err != nil {
		t.Fatal(err)
	}
	// 本人的 40 条作品早于其他用户的 5000 条，避免全站时间索引恰好在首页命中。
	for _, sql := range []string{
		`INSERT INTO community_skins(id,owner_id,name,design,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),CASE WHEN n<=40 THEN $1 ELSE 'catalog-user-'||(n%1000) END,
 '合成皮肤','{}',CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
		`INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,visibility,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),CASE WHEN n<=40 THEN $1 ELSE 'catalog-user-'||(n%1000) END,
 'package-'||n,'合成候选窗','1','MIT','x','preview.png',repeat('a',64),
 CASE WHEN n%7=0 THEN 'private' ELSE 'public' END,CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
		`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),CASE WHEN n<=40 THEN $1 ELSE 'catalog-user-'||(n%1000) END,
 'sound','plugin-'||n,'合成插件','1','MIT','x','x',repeat('b',64),CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
		`INSERT INTO community_resources(id,owner_id,kind,name,content,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),CASE WHEN n<=40 THEN $1 ELSE 'catalog-user-'||(n%1000) END,
 'dictionary','合成资源','{"entries":[]}',CASE WHEN n%10=0 THEN 'removed' ELSE 'approved' END,
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
	} {
		if _, err := db.pool.Exec(t.Context(), sql, owner.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(t.Context(), `ANALYZE auth_users; ANALYZE community_skins;
 ANALYZE community_candidate_skins; ANALYZE community_plugins; ANALYZE community_resources`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		table, key, query string
		candidate, sync   bool
		handler           func(*Service, http.ResponseWriter, *http.Request)
	}{
		{"community_skins", "skins", "", false, false, (*Service).communityList},
		{"community_candidate_skins", "skins", "", true, false, (*Service).communityCandidateList},
		{"community_candidate_skins", "skins", "&fields=sync", true, true, (*Service).communityCandidateList},
		{"community_plugins", "plugins", "", false, false, (*Service).communityPluginList},
		{"community_resources", "items", "&kind=dictionary", false, false, (*Service).resourceList},
	} {
		t.Run(tc.table+tc.query, func(t *testing.T) {
			a, trace := adminFilteredService(t, db, tc.table)
			var ids []string
			for n := 40; n > 0; n-- {
				if !tc.candidate || tc.sync || n%7 != 0 {
					ids = append(ids, fmt.Sprintf("00000000-0000-4000-8000-%012d", n))
				}
			}
			for _, offset := range []int{0, 20, 40} {
				t.Run(fmt.Sprint(offset), func(t *testing.T) {
					r := httptest.NewRequest("GET", fmt.Sprintf("/v1/community/catalog?scope=mine&offset=%d%s", offset, tc.query), nil)
					r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
					w := httptest.NewRecorder()
					tc.handler(a, w, r)
					var body map[string]json.RawMessage
					var items []struct {
						ID    string
						Owned bool
					}
					var more bool
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || json.Unmarshal(body[tc.key], &items) != nil || json.Unmarshal(body["has_more"], &more) != nil {
						t.Fatal("目录响应错误：", w.Code, w.Body.String())
					}
					want := ids[min(offset, len(ids)):min(offset+20, len(ids))]
					if items == nil || len(items) != len(want) || more != (offset+20 < len(ids)) {
						t.Fatal("本人作品分页长度错误：", w.Body.String())
					}
					for i, item := range items {
						if item.ID != want[i] || !item.Owned {
							t.Fatal("本人作品顺序或归属错误：", item, want[i])
						}
					}
					var query communityPageQuery
					select {
					case query = <-trace.queries:
					default:
						t.Fatal("没有记录到目录查询")
					}
					for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
						t.Run(mode, func(t *testing.T) {
							plan := adminFilteredExplain(t, db, query, mode)
							scanned := 0
							var visit func(adminFilteredPlan)
							visit = func(n adminFilteredPlan) {
								if n.Relation == tc.table {
									scanned += (n.Rows + n.Removed) * n.Loops
								}
								for _, child := range n.Plans {
									visit(child)
								}
							}
							visit(plan)
							t.Logf("目录扫描 %d 行", scanned)
							// 包含页内详情回查，仍只需读取本人的 40 条作品和最多 21 条详情。
							if scanned > 100 {
								t.Errorf("目录扫描 %d 行，超过 100 行预算", scanned)
							}
						})
					}
				})
			}
		})
	}
}
