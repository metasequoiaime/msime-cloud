package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCommunitySavedCategoryQueryPlans(t *testing.T) {
	db := testStore(t)
	reader := complete(t, db, Identity{"email", "saved-category-reader@example.test"})
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
	for _, sql := range []string{
		`INSERT INTO community_skin_saves(skin_id,user_id,created_at) SELECT id,$1,CASE WHEN right(id,12)::int<=40 THEN '2026-02-01'::timestamptz-((right(id,12)::int-1)/2)*interval '1 second' ELSE created_at+interval '1 year' END FROM community_skins`,
		`INSERT INTO community_candidate_skin_saves(skin_id,user_id,created_at) SELECT id,$1,CASE WHEN right(id,12)::int<=40 THEN '2026-02-01'::timestamptz-((right(id,12)::int-1)/2)*interval '1 second' ELSE created_at+interval '1 year' END FROM community_candidate_skins`,
		`INSERT INTO community_plugin_saves(pack_id,user_id,created_at) SELECT id,$1,CASE WHEN right(id,12)::int<=40 THEN '2026-02-01'::timestamptz-((right(id,12)::int-1)/2)*interval '1 second' ELSE created_at+interval '1 year' END FROM community_plugins`,
	} {
		if _, err := db.pool.Exec(t.Context(), sql, reader.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(t.Context(), `ANALYZE community_skin_saves;ANALYZE community_candidate_skin_saves;ANALYZE community_plugin_saves;ANALYZE auth_users; ANALYZE community_skins;
 ANALYZE community_candidate_skins; ANALYZE community_plugins`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		table, saves, key, query string
		count                    int
		descending               bool
		handler                  func(*Service, http.ResponseWriter, *http.Request)
	}{
		{"community_skins", "community_skin_saves", "skins", "category=nature", 40, false, (*Service).communityList},
		{"community_skins", "community_skin_saves", "skins", "category=tech", 0, false, (*Service).communityList},
		{"community_skins", "community_skin_saves", "skins", "category=other", 5000, true, (*Service).communityList},
		{"community_candidate_skins", "community_candidate_skin_saves", "skins", "category=nature", 40, false, (*Service).communityCandidateList},
		{"community_candidate_skins", "community_candidate_skin_saves", "skins", "category=tech", 0, false, (*Service).communityCandidateList},
		{"community_candidate_skins", "community_candidate_skin_saves", "skins", "category=other", 5000, true, (*Service).communityCandidateList},
		{"community_plugins", "community_plugin_saves", "plugins", "kind=helpcode&kinds=sound", 40, false, (*Service).communityPluginList},
		{"community_plugins", "community_plugin_saves", "plugins", "kind=effect&kinds=sound", 0, false, (*Service).communityPluginList},
		{"community_plugins", "community_plugin_saves", "plugins", "kind=sound", 5000, true, (*Service).communityPluginList},
	} {
		t.Run(tc.table+"/"+tc.query, func(t *testing.T) {
			a, trace := adminFilteredService(t, db, tc.table)
			for _, offset := range []int{0, 20, 40} {
				t.Run(fmt.Sprint(offset), func(t *testing.T) {
					r := httptest.NewRequest("GET", fmt.Sprintf("/v1/community/catalog?scope=saved&%s&offset=%d", tc.query, offset), nil)
					r.Header.Set("Authorization", "Bearer "+reader.AccessToken)
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
						number := offset + i + 1
						if tc.descending {
							number = 5040 - offset - i
						}
						if want := fmt.Sprintf("00000000-0000-4000-8000-%012d", number); item.ID != want {
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
							scanned := map[string]int{}
							var visit func(adminFilteredPlan)
							visit = func(n adminFilteredPlan) {
								if n.Relation == tc.table || n.Relation == tc.saves {
									scanned[n.Relation] += (n.Rows + n.Removed) * n.Loops
								}
								for _, child := range n.Plans {
									visit(child)
								}
							}
							visit(plan)
							// 作品和收藏关系都只应为匹配分类及页内详情读取，不能逐条回查全部收藏。
							for _, relation := range []string{tc.table, tc.saves} {
								t.Logf("%s 扫描 %d 行", relation, scanned[relation])
								if scanned[relation] > 100 {
									t.Errorf("%s 扫描 %d 行，超过 100 行预算", relation, scanned[relation])
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestCommunitySavedFilterKeepsUnknownValuesBound(t *testing.T) {
	db := testStore(t)
	for _, scope := range []string{"", "saved"} {
		for _, value := range []string{"nature", "unknown", "' OR true --", "nature' OR true --"} {
			t.Run(scope+"/"+value, func(t *testing.T) {
				var count int
				condition := communityTextFilter(value, "item.category", "$1") + communitySavedFilterValue(scope, value, "item.category", candidateSkinCategories)
				err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM (VALUES ('nature'),('other')) item(category) WHERE `+condition, value).Scan(&count)
				want := 0
				if value == "nature" {
					want = 1
				}
				if err != nil || count != want {
					t.Fatalf("分类匹配 %d 行，错误=%v；期望 %d 行", count, err, want)
				}
			})
		}
	}
}
