package account

import (
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestNotificationsReadAllLeavesUncommittedNotificationUnread(t *testing.T) {
	for _, mode := range []string{"now", "shown"} {
		t.Run(mode, func(t *testing.T) {
			a := notificationTestService(t)
			ctx := t.Context()
			pending, err := a.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Rollback(ctx)
			// 通知已经写入，但所属业务事务尚未提交，其他连接还看不到它。
			if err = a.Notify(ctx, pending, Notification{Kind: NotifyReport, Title: "尚未提交的举报"}); err != nil {
				t.Fatal(err)
			}
			if err = a.NotifyNow(ctx, Notification{Kind: NotifyIssue, Title: "已经显示的通知"}); err != nil {
				t.Fatal(err)
			}
			shown := listNotifications(t, a, "reader@example.test", "")
			if len(shown.Items) != 1 || shown.Unread != 1 {
				t.Fatalf("事务提交前的列表 = %+v", shown)
			}
			body := `{"all":true}`
			if mode == "shown" {
				body = fmt.Sprintf(`{"all":true,"up_to_id":"%d"}`, shown.Items[0].ID)
			}
			w := httptest.NewRecorder()
			a.AdminHTTP(w, notificationRequest("POST", "/api/notifications/read", body, "reader@example.test"))
			if w.Code != 200 {
				t.Fatalf("全部已读返回 %d：%s", w.Code, w.Body.String())
			}
			if err = pending.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := listNotifications(t, a, "reader@example.test", "")
			if len(got.Items) != 2 || got.Unread != 1 || !got.Items[0].Read || got.Items[1].Read || got.Items[1].Kind != NotifyReport {
				t.Fatalf("稍后提交的通知必须保持未读：%+v", got)
			}
			if unread, err := a.UnreadNotifications(ctx, "other@example.test"); err != nil || unread != 2 {
				t.Fatalf("其他管理员的未读数 = %d，错误 = %v", unread, err)
			}
			// 下一次全部已读可以覆盖已提交的通知，重复调用不会产生重复标记。
			for range 2 {
				w = httptest.NewRecorder()
				a.AdminHTTP(w, notificationRequest("POST", "/api/notifications/read", body, "reader@example.test"))
				if w.Code != 200 {
					t.Fatalf("再次全部已读返回 %d", w.Code)
				}
			}
			if unread, err := a.UnreadNotifications(ctx, "reader@example.test"); err != nil || unread != 0 {
				t.Fatalf("再次全部已读后的未读数 = %d，错误 = %v", unread, err)
			}
			if count := clipboardTestCount(t, a.store, `SELECT count(*) FROM admin_notification_reads`); count != 2 {
				t.Fatalf("重复操作后的标记数 = %d", count)
			}
		})
	}
}

func TestNotificationsReadAllPreservesLegacyWatermark(t *testing.T) {
	a := notificationTestService(t)
	ctx := t.Context()
	if err := a.NotifyNow(ctx, Notification{Kind: NotifyRelease}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.pool.Exec(ctx, `INSERT INTO admin_preferences(email,read_all_before) SELECT 'legacy@example.test',created_at FROM admin_notifications`); err != nil {
		t.Fatal(err)
	}
	if err := a.NotifyNow(ctx, Notification{Kind: NotifyIssue}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.AdminHTTP(w, notificationRequest("POST", "/api/notifications/read", `{"all":true}`, "legacy@example.test"))
	if w.Code != 200 {
		t.Fatalf("全部已读返回 %d", w.Code)
	}
	got := listNotifications(t, a, "legacy@example.test", "")
	if len(got.Items) != 2 || got.Unread != 0 || !got.Items[0].Read || !got.Items[1].Read {
		t.Fatalf("升级前后的通知都应保持已读：%+v", got)
	}
	if count := clipboardTestCount(t, a.store, `SELECT count(*) FROM admin_notification_reads`); count != 1 {
		t.Fatalf("旧水位覆盖的通知不应重复写入标记：%d", count)
	}
}
