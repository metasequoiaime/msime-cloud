package account

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCommunityListValidationOrder(t *testing.T) {
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
			for _, tc := range []struct{ name, query, code string }{
				{"偏移优先", "offset=bad&q=%ff&kind=bad&scope=bad", "invalid_offset"},
				{"搜索优先", "q=%ff&kind=bad&scope=bad", "invalid_search"},
				{"空字符", "q=one%00two", "invalid_search"},
				{"字节上限", "q=" + strings.Repeat("x", 129), "invalid_search"},
				{"范围优先于字段", "scope=bad", "invalid_scope"},
				{"默认分页", "", "invalid_fields"},
				{"空偏移", "offset=", "invalid_fields"},
				{"最大偏移", "offset=100000", "invalid_fields"},
				{"偏移越界", "offset=100001", "invalid_offset"},
				{"负偏移", "offset=-1", "invalid_offset"},
				{"搜索边界", "q=" + strings.Repeat("x", 128), "invalid_fields"},
				{"我的范围", "scope=mine", "invalid_fields"},
				{"收藏范围", "scope=saved", "invalid_fields"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := httptest.NewRecorder()
					r := httptest.NewRequest(http.MethodGet, "/v1/community/"+catalog.name+"?fields=unknown&"+tc.query, nil)
					catalog.handler(w, r)
					want := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`+"\n", tc.code, tc.code)
					if w.Code != http.StatusBadRequest || w.Body.String() != want || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("校验顺序或错误响应发生变化：状态=%d，响应=%q", w.Code, w.Body.String())
					}
				})
			}
		})
	}
	// 插件类型在通用范围之前校验，同时保留偏移、搜索先于类型的顺序。
	w := httptest.NewRecorder()
	service.communityPluginList(w, httptest.NewRequest(http.MethodGet, "/v1/community/plugins?kind=bad&scope=bad", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_kind"`) {
		t.Fatalf("插件校验优先级发生变化：%d %s", w.Code, w.Body.String())
	}
}

func TestCommunityListPage(t *testing.T) {
	for _, tc := range []struct {
		name, query  string
		offset       int
		search, code string
	}{
		{"默认值", "", 0, "", ""},
		{"空偏移", "offset=", 0, "", ""},
		{"边界偏移", "offset=100000", 100000, "", ""},
		{"带正号", "offset=%2B1", 1, "", ""},
		{"前导零", "offset=001", 1, "", ""},
		{"负偏移", "offset=-1", 0, "", "invalid_offset"},
		{"过大偏移", "offset=100001", 0, "", "invalid_offset"},
		{"溢出偏移", "offset=999999999999999999999999", 0, "", "invalid_offset"},
		{"偏移空白", "offset=+1", 0, "", "invalid_offset"},
		{"首个偏移", "offset=2&offset=bad", 2, "", ""},
		{"首个空偏移", "offset=&offset=bad", 0, "", ""},
		{"保留搜索空白", "q=" + url.QueryEscape(" 水杉 "), 0, " 水杉 ", ""},
		{"ASCII边界", "q=" + strings.Repeat("a", 128), 0, strings.Repeat("a", 128), ""},
		{"多字节边界", "q=" + url.QueryEscape(strings.Repeat("杉", 42)+"ab"), 0, strings.Repeat("杉", 42) + "ab", ""},
		{"多字节越界", "q=" + url.QueryEscape(strings.Repeat("杉", 43)), 0, "", "invalid_search"},
		{"空字符", "q=one%00two", 0, "", "invalid_search"},
		{"无效UTF8", "q=%ff", 0, "", "invalid_search"},
		{"替换字符", "q=" + url.QueryEscape("\ufffd"), 0, "\ufffd", ""},
		{"首个搜索值", "q=first&q=%ff", 0, "first", ""},
		{"偏移错误优先", "offset=bad&q=%ff", 0, "", "invalid_offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			offset, search, code := communityListPage(r.URL.Query())
			if offset != tc.offset || search != tc.search || code != tc.code {
				t.Fatalf("分页查询不符：offset=%d search=%q code=%q", offset, search, code)
			}
		})
	}
}

func TestCommunityListScope(t *testing.T) {
	for _, tc := range []struct {
		query, scope string
		ok           bool
	}{
		{"", "", true},
		{"scope=", "", true},
		{"scope=mine", "mine", true},
		{"scope=saved", "saved", true},
		{"scope=all", "all", false},
		{"scope=Mine", "Mine", false},
		{"scope=+mine", " mine", false},
		{"scope=saved&scope=bad", "saved", true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			scope, ok := communityListScope(httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil).URL.Query())
			if scope != tc.scope || ok != tc.ok {
				t.Fatalf("范围校验不符：scope=%q ok=%v", scope, ok)
			}
		})
	}
}

func TestCommunitySkinListCategory(t *testing.T) {
	for _, tc := range []struct {
		name, query, category string
		withCategory          bool
		code                  string
	}{
		{"默认值", "", "", false, ""},
		{"空分类", "category=", "", false, ""},
		{"筛选分类", "category=food", "food", false, ""},
		{"声明分类", "include=category", "", true, ""},
		{"筛选并声明", "category=tech&include=category", "tech", true, ""},
		{"分类大小写", "category=Food", "", false, "invalid_category"},
		{"分类空白", "category=+food", "", false, "invalid_category"},
		{"未知声明", "include=categories", "", false, "invalid_include"},
		{"分类错误优先", "category=bad&include=bad", "", false, "invalid_category"},
		{"首个分类", "category=food&category=bad&include=category", "food", true, ""},
		{"首个空分类", "category=&category=bad", "", false, ""},
		{"首个错误分类", "category=bad&category=food", "", false, "invalid_category"},
		{"首个声明", "include=category&include=bad", "", true, ""},
		{"首个空声明", "include=&include=bad", "", false, ""},
		{"首个错误声明", "include=bad&include=category", "", false, "invalid_include"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			category, withCategory, code := communitySkinListCategory(httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil).URL.Query())
			if category != tc.category || withCategory != tc.withCategory || code != tc.code {
				t.Fatalf("分类查询不符：category=%q withCategory=%v code=%q", category, withCategory, code)
			}
		})
	}
}
