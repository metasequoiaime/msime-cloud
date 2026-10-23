package server

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// releaseCancelResponseTransport 在指定写入的响应读取完成后取消请求，重现管理员断开连接。
type releaseCancelResponseTransport struct {
	base   http.RoundTripper
	method string
	path   string
	cancel context.CancelFunc
}

func (tr releaseCancelResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(r)
	if err == nil && r.Method == tr.method && r.URL.Path == tr.path {
		resp.Body = releaseCancelResponseBody{ReadCloser: resp.Body, cancel: tr.cancel}
	}
	return resp, err
}

type releaseCancelResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b releaseCancelResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func TestReleaseRecordsSurviveCancellation(t *testing.T) {
	const repo = "/repos/metasequoiaime/msime-windows"
	for _, tc := range []struct {
		name, path, body, method, upstreamPath, action, target string
		notifications                                          int
		restored                                               bool
	}{
		{"trigger", "/api/releases/windows/trigger", `{"version":"v0.5.5"}`, "POST", repo + "/actions/workflows/release.yml/dispatches", "release_trigger", "windows", 1, false},
		{"notes", "/api/releases/windows/windows-v0.5.4/notes", `{"body":"合成发布说明"}`, "PATCH", repo + "/releases/3", "release_notes", "windows-v0.5.4", 0, false},
		{"withdraw", "/api/releases/windows/windows-v0.5.4/withdraw", "", "PATCH", repo + "/releases/3", "release_withdraw", "windows-v0.5.4", 1, true},
		{"restore_latest", "/api/releases/windows/windows-v0.5.4/withdraw", "", "PATCH", repo + "/releases/2", "release_withdraw", "windows-v0.5.4", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, admin, schema := releaseDBServer(t)
			const email = "release-admin@example.test"
			const token = account.AdminTokenPrefix + "release-cancellation"
			s.adminStore = &adminMemoryStore{
				tokens:  map[string]account.AdminIdentity{token: {Email: email}},
				allowed: map[string]bool{email: true},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base := s.adminGitHub.HTTP.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			s.adminGitHub.HTTP.Transport = releaseCancelResponseTransport{base: base, method: tc.method, path: tc.upstreamPath, cancel: cancel}
			var result map[string]any
			r := releaseWrite(tc.path, tc.body).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+token)
			serveAdminJSON(t, s, r, 200, &result)
			if ctx.Err() == nil || f.count(tc.method+" "+tc.upstreamPath) != 1 {
				t.Fatal("未在上游写入成功后取消请求，或重复发送写入")
			}
			if tc.action == "release_notes" && f.lastBody("PATCH " + repo + "/releases/3")["body"] != "合成发布说明" {
				t.Fatal("未更新发布说明")
			}
			if tc.action == "release_withdraw" {
				if f.lastBody("PATCH " + repo + "/releases/3")["prerelease"] != true || result["latest_restored"] != tc.restored {
					t.Fatalf("撤回结果与已确认的写入不符：%+v", result)
				}
			}
			audits := releaseAudits(t, admin, schema)
			if len(audits) != 1 || audits[0].Action != tc.action || audits[0].Target != tc.target || audits[0].Detail["uncertain"] != nil {
				t.Errorf("请求取消后丢失成功操作的准确审计：%+v", audits)
			} else if tc.action == "release_withdraw" && (audits[0].Detail["latest_restored"] != tc.restored || audits[0].Detail["was_latest"] != true || audits[0].Detail["previous"] != "windows-v0.5.3") {
				t.Errorf("撤回审计未保留 latest 恢复的实际结果：%+v", audits[0].Detail)
			}
			var actor string
			err := admin.QueryRow(t.Context(), `SELECT actor FROM `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` WHERE action=$1`, tc.action).Scan(&actor)
			if err != nil || actor != "pat:"+email {
				t.Errorf("审计未保留操作者：actor=%q err=%v", actor, err)
			}
			if got := notificationRows(t, admin, schema); len(got) != tc.notifications {
				t.Errorf("请求取消后丢失发布通知：%v", got)
			}
		})
	}
}
