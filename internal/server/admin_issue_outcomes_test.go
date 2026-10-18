package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestAdminIssueActionUncertainOutcome(t *testing.T) {
	for _, action := range []string{"triage", "close", "comment"} {
		for _, outcome := range []string{"连接中断", "上游错误", "响应截断"} {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				s, f, conn, schema := issuesFixture(t)
				method, path := "PATCH", "/repos/"+issuesWinRepo+"/issues/10"
				switch action {
				case "triage":
					method, path = "POST", path+"/assignees"
				case "comment":
					method, path = "POST", path+"/comments"
				}
				upstream := uncertainAdminWriteUpstream(t, http.HandlerFunc(f.serve), method, path, outcome)
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
				body := ""
				if action == "comment" {
					body = `,"body":"合成回复，不应重复发送"`
				}
				status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", `{"action":"`+action+`","items":[{"repo":"`+issuesWinRepo+`","n":10}]`+body+`}`)
				got := f.get(issuesWinRepo, 10)
				if action == "triage" && (!slices.Contains(got.labels, issueTriagedLabel) || !slices.Contains(got.assignees, "houko")) {
					t.Errorf("结果未知时撤销了已确认的步骤：标签=%v，指派=%v", got.labels, got.assignees)
				}
				if action == "close" && got.state != "closed" {
					t.Errorf("未复现上游先完成写入：%+v", got)
				}
				if status != 502 || issueErrorCode(reply) != "github_uncertain" {
					t.Errorf("应提示先核对结果：%d %+v", status, reply)
				}
				writes := f.writeLog()
				wantWrites := 1
				if action == "triage" {
					wantWrites = 2
				}
				if len(writes) != wantWrites {
					t.Errorf("结果未知时重试或回滚了写入：%v", writes)
				}
				audits := issueAudits(t, conn, schema)
				if len(audits) != 1 || audits[0].action != "issue_"+action || audits[0].detail["uncertain"] != true {
					t.Errorf("未记录结果未知的操作：%+v", audits)
				}
			})
		}
	}
}

func TestAdminIssueBatchReportsUncertainOutcome(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			s, f, conn, schema := issuesFixture(t)
			upstream := uncertainAdminWriteUpstream(t, http.HandlerFunc(f.serve), "PATCH", "/repos/"+issuesWinRepo+"/issues/11", "连接中断")
			s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			first := 10
			if !success {
				first = 999
			}
			status, reply := issuesRequest(t, s, "POST", "/api/issues/actions", fmt.Sprintf(`{"action":"close","items":[{"repo":%q,"n":%d},{"repo":%q,"n":11}]}`, issuesWinRepo, first, issuesWinRepo))
			if success {
				failed, _ := reply["failed"].([]any)
				if status != 200 || reply["affected"] != 1.0 || len(failed) != 1 || failed[0].(map[string]any)["code"] != "github_uncertain" {
					t.Errorf("混合成功结果丢失未知状态：%d %+v", status, reply)
				}
			} else if status != 502 || issueErrorCode(reply) != "github_uncertain" {
				t.Errorf("其他失败掩盖了未知状态：%d %+v", status, reply)
			}
			audits := issueAudits(t, conn, schema)
			unknown := 0
			for _, audit := range audits {
				if audit.detail["uncertain"] == true && strings.HasSuffix(audit.target, "#11") {
					unknown++
				}
			}
			if unknown != 1 {
				t.Errorf("未记录批量操作中的未知状态：%+v", audits)
			}
		})
	}
}

// issueCancelResponseTransport 在写入响应读取完成后取消原请求，重现管理员断开连接。
type issueCancelResponseTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (tr issueCancelResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(r)
	if err == nil && r.Method == "PATCH" {
		resp.Body = issueCancelResponseBody{ReadCloser: resp.Body, cancel: tr.cancel}
	}
	return resp, err
}

type issueCancelResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b issueCancelResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestAdminIssueAuditSurvivesCancellation(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			s, f, conn, schema := issuesFixture(t)
			if uncertain {
				upstream := uncertainAdminWriteUpstream(t, http.HandlerFunc(f.serve), "PATCH", "/repos/"+issuesWinRepo+"/issues/10", "上游错误")
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := s.adminGitHub.HTTP.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			s.adminGitHub.HTTP.Transport = issueCancelResponseTransport{base: base, cancel: cancel}
			r := httptest.NewRequest("POST", "https://admin.example.com/api/issues/actions", strings.NewReader(`{"action":"close","items":[{"repo":"`+issuesWinRepo+`","n":10}]}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+issuesTestAdminToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			wantStatus := 200
			if uncertain {
				wantStatus = 502
			}
			if ctx.Err() == nil || w.Code != wantStatus {
				t.Errorf("未在写入后取消请求或错误处理取消：%v %d %s", ctx.Err(), w.Code, w.Body.String())
			}
			audits := issueAudits(t, conn, schema)
			if len(audits) != 1 || audits[0].actor != "legacy-token" || audits[0].action != "issue_close" || (audits[0].detail["uncertain"] == true) != uncertain {
				t.Fatalf("请求取消后丢失操作审计或操作者：%+v", audits)
			}
		})
	}
}
