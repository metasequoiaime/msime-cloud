package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCatalogTextValidationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		status      int
		code        string
	}{
		{"空查询", "", 503, "engine_unavailable"},
		{"搜索字节边界", "q=" + strings.Repeat("x", 1024), 503, "engine_unavailable"},
		{"搜索多字节边界", "q=" + url.QueryEscape(strings.Repeat("水", 341)+"x"), 503, "engine_unavailable"},
		{"搜索越界", "q=" + strings.Repeat("x", 1025), 400, "invalid_catalog_query"},
		{"分类字节边界", "category=" + strings.Repeat("x", 256), 503, "engine_unavailable"},
		{"分类越界", "category=" + strings.Repeat("x", 257), 400, "invalid_catalog_query"},
		{"保留其他控制字符", "q=%09%0A&category=%01", 503, "engine_unavailable"},
		{"搜索NUL", "q=a%00b", 400, "invalid_catalog_query"},
		{"分类NUL", "category=a%00b", 400, "invalid_catalog_query"},
		{"非法UTF8", "q=%FF", 400, "invalid_catalog_query"},
		// 旧规则检查两字段拼接后的编码，不能在抽取时改成分别检查。
		{"拼接编码兼容", "q=%E6%B0&category=%B4", 503, "engine_unavailable"},
		{"分页错误优先", "q=%00&offset=bad", 400, "invalid_offset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			r := httptest.NewRequest(http.MethodGet, "/v1/catalog/emoji?"+tc.query, nil)
			r.SetPathValue("kind", "emoji")
			w := httptest.NewRecorder()
			s.inputCatalog(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("响应为 %d %s，期望 %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
		})
	}
}
