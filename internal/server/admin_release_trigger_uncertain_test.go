package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestReleaseTriggerReportsUncertainDispatch(t *testing.T) {
	for _, outcome := range []string{"disconnect", "server_error", "truncated_body", "oversized_body", "cancelled_request", "audit_failure"} {
		t.Run(outcome, func(t *testing.T) {
			s, f, admin, schema := releaseDBServer(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if outcome == "audit_failure" {
				if _, err := admin.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "admin_audit"}.Sanitize()+` ADD CONSTRAINT reject_trigger CHECK (action<>'release_trigger')`); err != nil {
					t.Fatal(err)
				}
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/dispatches") {
					f.ServeHTTP(w, r)
					return
				}
				// 先记录已被上游接收的触发，再模拟确认响应丢失或损坏。
				f.ServeHTTP(httptest.NewRecorder(), r)
				switch outcome {
				case "cancelled_request":
					cancel()
					<-r.Context().Done()
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "truncated_body":
					w.Header().Set("Content-Length", "100")
					_, _ = io.WriteString(w, "synthetic-private-response")
				case "oversized_body":
					_, _ = io.WriteString(w, strings.Repeat("x", githubapp.MaxResponseBytes+1))
				default:
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "synthetic-private-response")
				}
			}))
			t.Cleanup(upstream.Close)
			gh := f.client(t)
			gh.APIURL, gh.HTTP = upstream.URL, upstream.Client()
			s.adminGitHub = gh
			w := httptest.NewRecorder()
			s.ServeHTTP(w, releaseWrite("/api/releases/windows/trigger", `{"version":"v0.5.7"}`).WithContext(ctx))
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"code":"workflow_uncertain"`) || strings.Contains(w.Body.String(), "synthetic-private-response") {
				t.Fatalf("未正确报告触发结果未知：%d %s", w.Code, w.Body.String())
			}
			if n := f.count("POST /repos/metasequoiaime/msime-windows/actions/workflows/release.yml/dispatches"); n != 1 {
				t.Fatalf("触发请求被重复发送：%d", n)
			}
			audits := releaseAudits(t, admin, schema)
			if outcome == "audit_failure" {
				if len(audits) != 0 {
					t.Fatal("审计失败仍写入记录", audits)
				}
			} else if len(audits) != 1 || audits[0].Action != "release_trigger" || audits[0].Target != "windows" || !jsonEqual(audits[0].Detail, map[string]any{"platform": "Windows", "version": "v0.5.7", "workflow": "release.yml", "uncertain": true}) {
				t.Fatal("触发结果未知缺少准确审计", audits)
			}
			var notifications int
			if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{schema, "admin_notifications"}.Sanitize()+` WHERE kind='release'`).Scan(&notifications); err != nil || notifications != 0 {
				t.Fatal("结果未知仍通知触发成功", notifications, err)
			}
		})
	}
}

func TestReleaseTriggerAcceptsRunResponse(t *testing.T) {
	s, f, admin, schema := releaseDBServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/dispatches") {
			f.ServeHTTP(w, r)
			return
		}
		f.ServeHTTP(httptest.NewRecorder(), r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"workflow_run_id":123,"run_url":"https://api.github.com/repos/example/test/actions/runs/123"}`)
	}))
	t.Cleanup(upstream.Close)
	gh := f.client(t)
	gh.APIURL, gh.HTTP = upstream.URL, upstream.Client()
	s.adminGitHub = gh
	serveAdminJSON(t, s, releaseWrite("/api/releases/windows/trigger", `{"version":"v0.5.7"}`), 200, nil)
	if audits := releaseAudits(t, admin, schema); len(audits) != 1 || audits[0].Detail["uncertain"] != nil {
		t.Fatal("成功的运行信息响应被误报为未知", audits)
	}
	if got := notificationRows(t, admin, schema); len(got) != 1 {
		t.Fatal("成功触发未通知", got)
	}
}
