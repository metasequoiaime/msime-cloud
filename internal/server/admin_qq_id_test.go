package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminQQNumberBoundariesBeforeDatabase(t *testing.T) {
	s, _ := adminRBACFixture(t)
	for _, route := range []struct {
		method, resource, prefix, action string
	}{
		{"GET", "drafts", "D", ""},
		{"GET", "doc-gaps", "F", ""},
		{"PATCH", "drafts", "D", ""},
		{"POST", "drafts", "D", "/issue"},
		{"POST", "drafts", "D", "/close"},
	} {
		for _, number := range []string{"1", "9223372036854775807", "", "0", "01", "+1", "9223372036854775808"} {
			path := "/api/qq-bot/" + route.resource + "/" + route.prefix + number + route.action
			t.Run(route.method+" "+path, func(t *testing.T) {
				w := httptest.NewRecorder()
				s.ServeHTTP(w, adminRequest(s, route.method, path, "", strings.Repeat("a", 40)))
				status, code := 400, "invalid_id"
				if number == "1" || number == "9223372036854775807" {
					status, code = 503, "database_unavailable"
				}
				var response struct {
					Error struct{ Code string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != status || response.Error.Code != code {
					t.Fatalf("编号验证顺序或错误发生变化：状态=%d，响应=%s，解析错误=%v", w.Code, w.Body.String(), err)
				}
			})
		}
	}
}
