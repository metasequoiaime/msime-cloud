package account

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminPagingQueryCompatibility(t *testing.T) {
	a := &Service{}
	for _, tc := range []struct {
		path, code string
		status     int
	}{
		{"/api/missing?page=", "not_found", 404},
		{"/api/missing?page=&page=0", "not_found", 404},
		{"/api/missing?page=01", "not_found", 404},
		{"/api/missing?page=%2B1", "not_found", 404},
		{"/api/missing?page=10000", "not_found", 404},
		{"/api/missing?page=10001", "invalid_page", 400},
		{"/api/missing?page=999999999999999999999999", "invalid_page", 400},
		{"/api/missing?page=+1", "invalid_page", 400},
		{"/api/missing?page=0&q=%FF", "invalid_page", 400},
		{"/api/notifications?limit=", "", 200},
		{"/api/notifications?limit=&limit=0", "", 200},
		{"/api/notifications?limit=01", "", 200},
		{"/api/notifications?limit=%2B1", "", 200},
		{"/api/notifications?limit=50", "", 200},
		{"/api/notifications?limit=51", "invalid_limit", 400},
		{"/api/notifications?limit=0&limit=1", "invalid_limit", 400},
		{"/api/notifications?limit=999999999999999999999999", "invalid_limit", 400},
		{"/api/notifications?limit=+1", "invalid_limit", 400},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			// 无邮箱的旧式令牌无需访问数据库，但仍必须先验证通知条数。
			a.AdminHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status || (tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatalf("响应为 %d %s，期望 %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
		})
	}
}
