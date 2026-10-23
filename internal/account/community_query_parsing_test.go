package account

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var communityQueryValuesSink url.Values

func TestCommunityQueryParsingAllocationBudget(t *testing.T) {
	a := &Service{}
	for _, tc := range []struct {
		name, query, code string
		status            int
		handler           func(http.ResponseWriter, *http.Request)
	}{
		{"skins", "offset=2&q=needle&scope=mine", "user_session_required", 401, a.communityList},
		{"candidate", "offset=2&q=needle&scope=mine", "user_session_required", 401, a.communityCandidateList},
		{"plugins", "offset=2&q=needle&scope=mine", "user_session_required", 401, a.communityPluginList},
		{"resources", "kind=phrase&q=needle&scope=mine", "login_required", 401, a.resourceList},
		{"skin-detail", "fields=moderation&include=bad", "invalid_include", 400, a.communityDetail},
		{"candidate-detail", "fields=moderation&include=bad", "invalid_include", 400, a.communityCandidateDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := httptest.NewRequest("GET", "/?"+tc.query, nil)
			extra := httptest.NewRequest("GET", "/?"+tc.query+"&probe_a=a&probe_b=b&probe_c=c&probe_d=d", nil)
			parse := func(r *http.Request) float64 {
				return testing.AllocsPerRun(100, func() { communityQueryValuesSink = r.URL.Query() })
			}
			measure := func(r *http.Request) float64 {
				var w *httptest.ResponseRecorder
				allocs := testing.AllocsPerRun(100, func() { w = httptest.NewRecorder(); tc.handler(w, r) })
				if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
					t.Fatalf("响应变化：%d %s", w.Code, w.Body.String())
				}
				return allocs
			}
			growth, parseGrowth := measure(extra)-measure(base), parse(extra)-parse(base)
			t.Logf("四个未知参数的分配增量：请求 %.0f，单次解析 %.0f", growth, parseGrowth)
			// 测量未知参数的增量，避免把响应编码或鉴权分配当成查询解析成本；给 race 下的编码池留一次波动余量。
			if parseGrowth <= 1 || growth > parseGrowth+1 {
				t.Fatalf("请求新增 %.0f 次分配，超过单次查询解析预算 %.0f", growth, parseGrowth+1)
			}
		})
	}
}

func TestCommunityQueryParsingCompatibility(t *testing.T) {
	a := &Service{}
	for _, handler := range []func(http.ResponseWriter, *http.Request){a.communityList, a.communityCandidateList, a.communityPluginList} {
		for _, tc := range []struct {
			query, code string
			status      int
		}{
			{"offset=2&offset=bad&q=needle&scope=mine", "user_session_required", 401},
			{"offset=bad&offset=2&q=needle&scope=mine", "invalid_offset", 400},
			{"q=needle&q=%ff&scope=mine", "user_session_required", 401},
			{"q=%ff&q=needle&scope=mine", "invalid_search", 400},
			{"scope=mine&scope=bad", "user_session_required", 401},
			{"scope=bad&scope=mine", "invalid_scope", 400},
			{"sc%6fpe=mine&fields=moderation&fields=bad", "user_session_required", 401},
			{"scope=mine&ignored=%zz", "user_session_required", 401},
			{"scope=mine&ignored=x;y", "user_session_required", 401},
			{"scope=mine&fields=bad&category=bad&include=bad", "invalid_fields", 400},
		} {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest("GET", "/?"+tc.query, nil))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("查询 %q 的响应变化：%d %s", tc.query, w.Code, w.Body.String())
			}
		}
	}
}

func BenchmarkCommunityQueryParsing(b *testing.B) {
	a := &Service{}
	for _, tc := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"skins", a.communityList},
		{"candidate", a.communityCandidateList},
		{"plugins", a.communityPluginList},
	} {
		for _, query := range []struct{ name, value string }{
			{"short", "offset=2&q=needle&scope=mine"},
			{"extra", "offset=2&q=needle&scope=mine&probe_a=a&probe_b=b&probe_c=c&probe_d=d"},
			{"escaped", "offset=2&q=" + url.QueryEscape("水杉 + needle") + "&scope=mine"},
		} {
			b.Run(tc.name+"/"+query.name, func(b *testing.B) {
				r := httptest.NewRequest("GET", "/?"+query.value, nil)
				b.ReportAllocs()
				for b.Loop() {
					w := httptest.NewRecorder()
					tc.handler(w, r)
					if w.Code != http.StatusUnauthorized {
						b.Fatal(w.Code)
					}
				}
			})
		}
	}
}
