package account

import (
	"context"
	"net/http"
	"testing"
)

func TestNoticeAuditFailureDoesNotBroadcast(t *testing.T) {
	for _, draft := range []bool{false, true} {
		name := "直接发布"
		if draft {
			name = "发布草稿"
		}
		t.Run(name, func(t *testing.T) {
			a, db := noticeTestService(t)
			broadcaster := &fakeBroadcaster{}
			a.ConfigureNoticeBroadcaster(broadcaster)
			ctx := adminTestContext(t.Context(), "google:owner:owner@example.test")
			value := `{"title":"测试公告","body":"合成正文","targets":["all"],"channels":["app","telegram"]}`
			request := `{"action":"publish_notice","value":` + value + `}`
			if draft {
				status, out := noticeAction(t, a, ctx, `{"action":"save_notice_draft","value":`+value+`}`)
				if status != http.StatusOK {
					t.Fatal(status, out)
				}
				request = `{"action":"publish_notice","id":"` + out["id"].(string) + `"}`
			}
			if _, err := db.pool.Exec(t.Context(), `ALTER TABLE admin_audit ADD CONSTRAINT reject_notice_audit CHECK(action<>'publish_notice')`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_audit DROP CONSTRAINT IF EXISTS reject_notice_audit`); err != nil {
					t.Error(err)
				}
			})
			// 审计失败和管理员重试都不能把未发布的公告发送到外部频道。
			for range 2 {
				if status, out := noticeAction(t, a, ctx, request); status != http.StatusServiceUnavailable || errorCode(out) != "auth_unavailable" {
					t.Fatal(status, out)
				}
			}
			if len(broadcaster.sent) != 0 {
				t.Fatalf("审计失败仍广播了 %d 次公告", len(broadcaster.sent))
			}
			if items := publicNotices(t, a, "/v1/notices"); len(items) != 0 {
				t.Fatal("审计失败仍发布到公共列表", items)
			}
			if n := auditCount(t, db, "publish_notice"); n != 0 {
				t.Fatal("失败的发布留下审计", n)
			}
			if _, err := db.pool.Exec(t.Context(), `ALTER TABLE admin_audit DROP CONSTRAINT reject_notice_audit`); err != nil {
				t.Fatal(err)
			}
			if status, out := noticeAction(t, a, ctx, request); status != http.StatusOK {
				t.Fatal("审计恢复后无法发布", status, out)
			}
			if len(broadcaster.sent) != 1 || auditCount(t, db, "publish_notice") != 1 {
				t.Fatal("恢复后的发布未恰好广播并审计一次", len(broadcaster.sent))
			}
		})
	}
}
