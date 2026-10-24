package account

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func TestCommunityListOptions(t *testing.T) {
	for _, catalog := range []struct {
		name   string
		fields func(url.Values) (map[string]bool, bool)
		sync   bool
	}{
		{"目录", communityCatalogFields, false},
		{"候选窗", candidateFields, true},
	} {
		t.Run(catalog.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, query, scope, code string
				fields                   []string
			}{
				{"默认值", "", "", "", nil},
				{"我的范围", "scope=mine", "mine", "", nil},
				{"收藏范围", "scope=saved", "saved", "", nil},
				{"首个范围", "scope=saved&scope=bad", "saved", "", nil},
				{"首个空范围", "scope=&scope=bad", "", "", nil},
				{"首个错误范围", "scope=bad&scope=saved", "", "invalid_scope", nil},
				{"范围大小写", "scope=Mine", "", "invalid_scope", nil},
				{"范围空白", "scope=+mine", "", "invalid_scope", nil},
				{"审核字段", "fields=moderation", "", "", []string{"moderation"}},
				{"收藏字段", "scope=mine&fields=saved", "mine", "", []string{"saved"}},
				{"组合字段", "scope=saved&fields=moderation,saved", "saved", "", []string{"moderation", "saved"}},
				{"重复声明", "fields=saved,saved", "", "", []string{"saved"}},
				{"首个字段", "fields=saved&fields=bad", "", "", []string{"saved"}},
				{"首个空字段", "fields=&fields=bad", "", "", nil},
				{"首个错误字段", "fields=bad&fields=saved", "", "invalid_fields", nil},
				{"字段空白", "fields=+saved", "", "invalid_fields", nil},
				{"空字段段", "fields=saved,", "", "invalid_fields", nil},
				{"范围错误优先", "scope=bad&fields=bad", "", "invalid_scope", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					query, err := url.ParseQuery(tc.query)
					if err != nil {
						t.Fatal(err)
					}
					before := query.Encode()
					scope, fields, code := communityListOptions(query, catalog.fields)
					var want map[string]bool
					if len(tc.fields) > 0 {
						want = make(map[string]bool, len(tc.fields))
						for _, field := range tc.fields {
							want[field] = true
						}
					}
					if scope != tc.scope || code != tc.code || !reflect.DeepEqual(fields, want) {
						t.Fatalf("选项不符：scope=%q fields=%v code=%q", scope, fields, code)
					}
					if query.Encode() != before {
						t.Fatal("解析修改了调用方的查询值")
					}
				})
			}
			t.Run("同步字段策略", func(t *testing.T) {
				_, fields, code := communityListOptions(url.Values{"fields": {"sync,saved"}}, catalog.fields)
				if catalog.sync {
					if code != "" || !reflect.DeepEqual(fields, map[string]bool{"sync": true, "saved": true}) {
						t.Fatalf("候选窗同步声明丢失：fields=%v code=%q", fields, code)
					}
				} else if code != "invalid_fields" || fields != nil {
					t.Fatalf("目录不应接受同步声明：fields=%v code=%q", fields, code)
				}
			})
		})
	}
}

func TestCommunityCatalogFieldsOwnership(t *testing.T) {
	if fields, ok := communityCatalogFields(nil); !ok || fields != nil {
		t.Fatalf("未声明字段时应保留 nil：fields=%v ok=%v", fields, ok)
	}
	query := url.Values{"fields": {"moderation,saved"}}
	first, ok := communityCatalogFields(query)
	if !ok {
		t.Fatal("合法字段被拒绝")
	}
	first["saved"] = false
	second, ok := communityCatalogFields(query)
	if !ok || !reflect.DeepEqual(second, map[string]bool{"moderation": true, "saved": true}) {
		t.Fatalf("字段集合在请求之间共享：fields=%v ok=%v", second, ok)
	}
}

func TestCommunityListOptionsHandlers(t *testing.T) {
	service := &Service{}
	for _, catalog := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"skins", service.communityList},
		{"candidate-skins", service.communityCandidateList},
		{"plugins", service.communityPluginList},
	} {
		t.Run(catalog.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, query, code string
			}{
				{"同步字段", "fields=sync&category=bad", "invalid_fields"},
				{"插件类型优先", "kind=bad&scope=bad&fields=bad", "invalid_scope"},
				{"资源声明优先", "resources=bad&offset=bad&scope=bad&fields=bad", "invalid_offset"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					wantCode := tc.code
					if tc.name == "同步字段" && catalog.name == "candidate-skins" {
						wantCode = "invalid_category"
					}
					if tc.name == "插件类型优先" && catalog.name == "plugins" {
						wantCode = "invalid_kind"
					}
					if tc.name == "资源声明优先" && catalog.name == "candidate-skins" {
						wantCode = "invalid_fields"
					}
					w := httptest.NewRecorder()
					catalog.handler(w, httptest.NewRequest(http.MethodGet, "/v1/community/"+catalog.name+"?"+tc.query, nil))
					want := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`+"\n", wantCode, wantCode)
					if w.Code != http.StatusBadRequest || w.Body.String() != want || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("错误优先级或响应改变：状态=%d，响应=%q", w.Code, w.Body.String())
					}
				})
			}
		})
	}
}
