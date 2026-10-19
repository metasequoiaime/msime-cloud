package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommunityCategoryListsUseFilterIndex(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `INSERT INTO auth_users(id)
 SELECT 'category-user-'||n FROM generate_series(0,999) n`); err != nil {
		t.Fatal(err)
	}
	// 匹配分类的 40 条作品早于其余 5000 条，避免全站时间索引恰好在首页命中。
	for _, sql := range []string{
		`INSERT INTO community_skins(id,owner_id,name,design,category,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),'category-user-'||(n%1000),'合成皮肤','{}',
 CASE WHEN n<=40 THEN 'nature' ELSE 'other' END,'approved',
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
		`INSERT INTO community_candidate_skins(id,owner_id,package_id,name,version,license_assets,manifest,preview_path,request_sha256,category,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),'category-user-'||(n%1000),'package-'||n,
 '合成候选窗','1','MIT','x','preview.png',repeat('a',64),CASE WHEN n<=40 THEN 'nature' ELSE 'other' END,'approved',
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
		`INSERT INTO community_plugins(id,owner_id,kind,plugin_id,name,version,license,manifest,archive,request_sha256,moderation,created_at)
 SELECT '00000000-0000-4000-8000-'||lpad(n::text,12,'0'),'category-user-'||(n%1000),
 CASE WHEN n<=40 THEN 'helpcode' ELSE 'sound' END,'plugin-'||n,'合成插件','1','MIT','x','x',repeat('b',64),'approved',
 '2026-01-01'::timestamptz+n*interval '1 second' FROM generate_series(1,5040) n`,
	} {
		if _, err := db.pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(t.Context(), `ANALYZE auth_users; ANALYZE community_skins;
 ANALYZE community_candidate_skins; ANALYZE community_plugins`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		table, key, query string
		count             int
		handler           func(*Service, http.ResponseWriter, *http.Request)
	}{
		{"community_skins", "skins", "category=nature", 40, (*Service).communityList},
		{"community_skins", "skins", "category=tech", 0, (*Service).communityList},
		{"community_candidate_skins", "skins", "category=nature", 40, (*Service).communityCandidateList},
		{"community_candidate_skins", "skins", "category=tech", 0, (*Service).communityCandidateList},
		{"community_plugins", "plugins", "kind=helpcode&kinds=sound", 40, (*Service).communityPluginList},
		{"community_plugins", "plugins", "kind=effect&kinds=sound", 0, (*Service).communityPluginList},
	} {
		t.Run(tc.table+"/"+tc.query, func(t *testing.T) {
			a, trace := adminFilteredService(t, db, tc.table)
			for _, offset := range []int{0, 20, 40} {
				t.Run(fmt.Sprint(offset), func(t *testing.T) {
					r := httptest.NewRequest("GET", fmt.Sprintf("/v1/community/catalog?%s&offset=%d", tc.query, offset), nil)
					w := httptest.NewRecorder()
					tc.handler(a, w, r)
					var body map[string]json.RawMessage
					var items []struct{ ID string }
					var more bool
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || json.Unmarshal(body[tc.key], &items) != nil || json.Unmarshal(body["has_more"], &more) != nil {
						t.Fatal("分类目录响应错误：", w.Code, w.Body.String())
					}
					want := min(offset+20, tc.count) - min(offset, tc.count)
					if items == nil || len(items) != want || more != (offset+20 < tc.count) {
						t.Fatal("分类目录分页长度错误：", w.Body.String())
					}
					for i, item := range items {
						if want := fmt.Sprintf("00000000-0000-4000-8000-%012d", tc.count-offset-i); item.ID != want {
							t.Fatal("分类目录顺序或筛选错误：", item, want)
						}
					}
					var query communityPageQuery
					select {
					case query = <-trace.queries:
					default:
						t.Fatal("没有记录到分类目录查询")
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
							t.Logf("分类目录扫描 %d 行", scanned)
							// 包含页内详情回查，仍只需读取匹配分类的作品和最多 21 条详情。
							if scanned > 100 {
								t.Errorf("分类目录扫描 %d 行，超过 100 行预算", scanned)
							}
						})
					}
				})
			}
		})
	}
}
