package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSkinCatalogQueryCompatibility(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name, query, canonical string
		status                 int
	}{
		{"无筛选", "", "", 200},
		{"空筛选", "layout=&theme=", "", 200},
		{"双筛选", "layout=horizontal&theme=dark", "layout=horizontal&theme=dark", 200},
		{"编码键和值", "lay%6fut=horizontal&theme=d%61rk", "layout=horizontal&theme=dark", 200},
		{"重复值取首项", "layout=vertical&layout=invalid&theme=light&theme=invalid", "layout=vertical&theme=light", 200},
		{"空首项保留", "layout=&layout=horizontal&theme=&theme=dark", "", 200},
		{"无效首项拒绝", "layout=invalid&layout=horizontal", "", 400},
		{"未知参数拒绝", "layout=horizontal&theme=dark&extra=x", "", 400},
		{"编码未知参数拒绝", "layout=horizontal&%65xtra=x", "", 400},
		{"无效转义项忽略", "layout=%zz&theme=dark", "theme=dark", 200},
		{"分号项忽略", "layout=horizontal;extra=x&theme=dark", "theme=dark", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.skinCatalog(w, httptest.NewRequest("GET", "/v1/skins?"+tc.query, nil))
			if w.Code != tc.status {
				t.Fatalf("状态码 %d，期望 %d：%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 400 {
				if !strings.Contains(w.Body.String(), `"code":"invalid_skin_filter"`) {
					t.Fatal(w.Body.String())
				}
				return
			}
			canonical := httptest.NewRecorder()
			s.skinCatalog(canonical, httptest.NewRequest("GET", "/v1/skins?"+tc.canonical, nil))
			if canonical.Code != 200 || w.Body.String() != canonical.Body.String() || w.Header().Get("Content-Type") != canonical.Header().Get("Content-Type") {
				t.Fatalf("与标准查询的响应不同：%s / %s", w.Body.String(), canonical.Body.String())
			}
		})
	}
}

func TestSkinCatalogQueryAllocationBudget(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/v1/skins?lay%6fut=horizontal&theme=d%61rk&extra=x", nil)
	var w *httptest.ResponseRecorder
	allocations := testing.AllocsPerRun(100, func() {
		w = httptest.NewRecorder()
		s.skinCatalog(w, r)
	})
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_skin_filter"`) {
		t.Fatalf("未执行真实筛选校验：%d %s", w.Code, w.Body.String())
	}
	// 预算覆盖真实处理函数和错误响应，留出余量并检出重复解析查询参数。
	if allocations > 35 {
		t.Fatalf("皮肤目录筛选分配 %.0f 次，预算 35 次", allocations)
	}
	t.Logf("真实皮肤目录处理函数：%.0f 次分配", allocations)
}

func BenchmarkSkinCatalogQuery(b *testing.B) {
	s := &Server{}
	for _, tc := range []struct {
		name, query string
		status      int
	}{
		{"filtered", "lay%6fut=horizontal&theme=d%61rk", 200},
		{"invalid", "lay%6fut=horizontal&theme=d%61rk&extra=x", 400},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := httptest.NewRequest("GET", "/v1/skins?"+tc.query, nil)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				s.skinCatalog(w, r)
				if w.Code != tc.status {
					b.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
}
