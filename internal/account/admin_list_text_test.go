package account

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminListRejectsInvalidDatabaseText(t *testing.T) {
	a := &Service{store: testStore(t)}
	for _, tc := range []struct {
		name, path, param, value, code string
	}{
		{"search_nul", "users", "q", "prefix\x00suffix", "invalid_search"},
		{"search_utf8", "users", "q", "prefix\xffsuffix", "invalid_search"},
		{"exact_nul", "downloads", "platform", "win\x00", "invalid_filter"},
		{"exact_utf8", "downloads", "version", "v\xff", "invalid_filter"},
		{"contains_nul", "audit", "actor", "user\x00", "invalid_filter"},
		{"contains_utf8", "audit", "actor", "user\xff", "invalid_filter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/"+tc.path+"?"+tc.param+"="+url.QueryEscape(tc.value), nil)
			a.AdminHTTP(w, r)
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	// 合法中文、空查询和恰好达到字节上限的值继续进入正常查询。
	for _, query := range []string{
		"users?q=" + url.QueryEscape("水杉"),
		"users?q=",
		"users?q=" + strings.Repeat("a", 200),
		"downloads?platform=" + strings.Repeat("a", 32),
		"audit?actor=" + url.QueryEscape("维护者"),
	} {
		w := httptest.NewRecorder()
		a.AdminHTTP(w, httptest.NewRequest("GET", "/api/"+query, nil))
		if w.Code != 200 {
			t.Fatalf("query=%s status=%d body=%s", query, w.Code, w.Body.String())
		}
	}
}
