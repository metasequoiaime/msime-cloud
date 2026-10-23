package account

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var dictionaryExportQuerySink url.Values

func TestDictionaryExportQueryAllocationBudget(t *testing.T) {
	a, user := dictionaryExportFixture(t, 0)
	base := dictionaryExportRequest(user.AccessToken, "quick", "standard&format=standard")
	extra := dictionaryExportRequest(user.AccessToken, "quick", "standard&format=standard&probe_a=a&probe_b=b&probe_c=c&probe_d=d")
	measure := func(r *http.Request) float64 {
		var w *httptest.ResponseRecorder
		allocs := testing.AllocsPerRun(100, func() { w = httptest.NewRecorder(); a.dictionaryExport(w, r) })
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_dictionary_format"`) {
			t.Fatalf("重复格式没有按原规则拒绝：%d %s", w.Code, w.Body.String())
		}
		return allocs
	}
	parse := func(r *http.Request) float64 {
		return testing.AllocsPerRun(100, func() { dictionaryExportQuerySink = r.URL.Query() })
	}
	growth, parseGrowth := measure(extra)-measure(base), parse(extra)-parse(base)
	t.Logf("四个未知参数的分配增量：请求 %.0f，单次解析 %.0f", growth, parseGrowth)
	// 真实鉴权请求只增加无业务影响的未知参数；给 race 下的响应编码池留一次分配波动余量。
	if parseGrowth <= 1 || growth > parseGrowth+1 {
		t.Fatalf("请求新增 %.0f 次分配，超过单次查询解析预算 %.0f", growth, parseGrowth+1)
	}
}

func TestDictionaryExportQueryValidation(t *testing.T) {
	a, user := dictionaryExportFixture(t, 0)
	for _, tc := range []struct {
		name, query, kind, code string
		status                  int
	}{
		{"默认格式", "", "quick", "", 200},
		{"空格式", "format=", "quick", "", 200},
		{"标准格式", "format=standard", "quick", "", 200},
		{"Windows格式", "format=windows", "quick", "", 200},
		{"转义参数名", "for%6dat=standard", "quick", "", 200},
		{"转义参数值", "format=%73tandard", "quick", "", 200},
		{"忽略坏编码", "format=standard&ignored=%zz", "quick", "", 200},
		{"忽略分号", "format=standard&ignored=x;y", "quick", "", 200},
		{"重复标准格式", "format=standard&format=standard", "quick", "invalid_dictionary_format", 400},
		{"重复空格式", "format=&format=", "quick", "invalid_dictionary_format", 400},
		{"首个空格式", "format=&format=windows", "quick", "invalid_dictionary_format", 400},
		{"转义重复参数名", "format=standard&for%6dat=standard", "quick", "invalid_dictionary_format", 400},
		{"第二格式无效", "format=standard&format=bad", "quick", "invalid_dictionary_format", 400},
		{"首个格式无效", "format=bad&format=standard", "quick", "invalid_dictionary_format", 400},
		{"无效UTF8", "format=%ff", "quick", "invalid_dictionary_format", 400},
		{"格式大小写", "format=Standard", "quick", "invalid_dictionary_format", 400},
		{"种类错误优先", "format=bad&format=standard", "bad", "not_found", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := dictionaryExportRequest(user.AccessToken, tc.kind, "")
			r.URL.RawQuery = tc.query
			w := httptest.NewRecorder()
			a.dictionaryExport(w, r)
			if w.Code != tc.status || (tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`)) || (tc.code == "" && w.Body.Len() != 0) {
				t.Fatalf("格式校验响应变化：%d %s", w.Code, w.Body.String())
			}
		})
	}
	// 缺少会话时仍优先返回鉴权错误，不能提前处理格式或词库种类。
	r := dictionaryExportRequest("", "bad", "bad&format=standard")
	r.Header.Del("Authorization")
	w := httptest.NewRecorder()
	a.dictionaryExport(w, r)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"code":"user_session_required"`) {
		t.Fatalf("鉴权优先级变化：%d %s", w.Code, w.Body.String())
	}
}

func BenchmarkDictionaryExportQuery(b *testing.B) {
	a, user := dictionaryExportFixture(b, 0)
	for _, tc := range []struct{ name, query string }{
		{"short", "standard&format=standard"},
		{"extra", "standard&format=standard&probe_a=a&probe_b=b&probe_c=c&probe_d=d"},
		{"escaped", "%73tandard&format=standard"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := dictionaryExportRequest(user.AccessToken, "quick", tc.query)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				a.dictionaryExport(w, r)
				if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_dictionary_format"`) {
					b.Fatalf("格式校验响应变化：%d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
