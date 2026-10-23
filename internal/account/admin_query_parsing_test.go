package account

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var adminQueryValuesSink url.Values

func TestAdminListQueryParsingAllocationBudget(t *testing.T) {
	a := &Service{}
	for _, path := range []string{
		"/api/skins?page=2&q=needle&role=user",
		"/api/users?page=2&q=needle&visibility=public",
		"/api/downloads?page=2&q=needle&role=user",
	} {
		t.Run(path, func(t *testing.T) {
			// 未知参数按现有规则忽略；它们的分配增量应只包含一次查询串解析。
			base := httptest.NewRequest("GET", path, nil)
			extra := httptest.NewRequest("GET", path+"&probe_a=a&probe_b=b&probe_c=c&probe_d=d", nil)
			parseGrowth := testing.AllocsPerRun(100, func() { adminQueryValuesSink = extra.URL.Query() }) - testing.AllocsPerRun(100, func() { adminQueryValuesSink = base.URL.Query() })
			measure := func(withExtra bool) float64 {
				r := base
				if withExtra {
					r = extra
				}
				var w *httptest.ResponseRecorder
				allocs := testing.AllocsPerRun(100, func() { w = httptest.NewRecorder(); a.AdminHTTP(w, r) })
				if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_filter"`) {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				return allocs
			}
			growth := measure(true) - measure(false)
			t.Logf("新增参数分配增量：处理请求 %.0f，单次解析 %.0f", growth, parseGrowth)
			// race 模式下 JSON 编码器的 sync.Pool 会随机丢弃对象，平均分配取整可能产生 1 次波动。
			// 一次额外解析至少多分配 4 次，因此该余量仍会检出重复解析。
			if parseGrowth <= 1 || growth > parseGrowth+1 {
				t.Fatalf("query parsing allocated %.0f additional times, want at most %.0f", growth, parseGrowth+1)
			}
		})
	}
}

func TestAdminListQueryParsingValidation(t *testing.T) {
	a := &Service{}
	for _, tc := range []struct {
		path, code string
		status     int
	}{
		{"/api/skins?page=2&page=bad&q=needle&role=user", "invalid_filter", 400},
		{"/api/skins?page=bad&page=2", "invalid_page", 400},
		{"/api/skins?q=needle&q=%FF&role=user", "invalid_filter", 400},
		{"/api/skins?q=%FF&q=needle&role=user", "invalid_search", 400},
		{"/api/skins?%72ole=user", "invalid_filter", 400},
		{"/api/skins?ignored=%zz&role=user", "invalid_filter", 400},
		{"/api/skins?role=user&role=", "invalid_filter", 400},
		{"/api/missing?page=0", "invalid_page", 400},
		{"/api/missing?q=%FF", "invalid_search", 400},
		{"/api/missing?q=needle", "not_found", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.AdminHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func BenchmarkAdminListQueryParsing(b *testing.B) {
	a := &Service{}
	for _, tc := range []struct{ name, path string }{
		{"short", "/api/skins?page=2&q=needle&role=user"},
		{"extra", "/api/skins?page=2&q=needle&role=user&probe_a=a&probe_b=b&probe_c=c&probe_d=d"},
		{"escaped", "/api/skins?page=2&q=" + url.QueryEscape("水杉 + needle") + "&role=user"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := httptest.NewRequest("GET", tc.path, nil)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				a.AdminHTTP(w, r)
				if w.Code != 400 {
					b.Fatal(w.Code)
				}
			}
		})
	}
}
