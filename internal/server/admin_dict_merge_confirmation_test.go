package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDictPRApproveRequiresMergeConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"malformed", `{"merged":true`},
		{"null", `null`},
		{"missing_fields", `{}`},
		{"not_merged", `{"merged":false,"sha":"merge12"}`},
		{"missing_merged", `{"sha":"merge12"}`},
		{"missing_sha", `{"merged":true}`},
		{"empty_sha", `{"merged":true,"sha":""}`},
		{"wrong_type", `{"merged":"true","sha":"merge12"}`},
	} {
		for _, trim := range []bool{false, true} {
			name := tc.name + "/all"
			body := `{"head_sha":"head12"}`
			if trim {
				name = tc.name + "/trim"
				body = `{"keep":[0,4,6],"head_sha":"head12"}`
			}
			t.Run(name, func(t *testing.T) {
				s, f, conn, schema := dictPRServer(t, "")
				mergeCalls := 0
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPut && r.URL.Path == "/repos/"+dictTestRepo+"/pulls/12/merge" {
						mergeCalls++
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(tc.body))
						return
					}
					f.serve(w, r)
				}))
				t.Cleanup(upstream.Close)
				s.adminGitHub.APIURL = upstream.URL
				s.adminGitHub.HTTP = upstream.Client()

				w := dictCall(s, "POST", "/api/dict-prs/12/approve", body)
				if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"github_uncertain"`) {
					t.Errorf("未确认合并时应返回 github_uncertain，实际为 %d %s", w.Code, w.Body.String())
				}
				if mergeCalls != 1 {
					t.Fatalf("合并请求次数 = %d，应为 1", mergeCalls)
				}
				if p := f.pull(12); p.merged || p.state != "open" {
					t.Fatalf("未确认的合并改变了 PR 状态：%+v", p)
				}
				rows := dictAudit(t, conn, schema)
				if trim {
					if len(rows) != 1 || rows[0] != (dictAuditRow{"dict_pr_trim", "12", "3", "4", "", "legacy-token"}) {
						t.Fatalf("只能保留已完成精简的审计记录：%+v", rows)
					}
				} else if len(rows) != 0 {
					t.Fatalf("未确认合并不应记录通过：%+v", rows)
				}
			})
		}
	}
}
