package account

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNoticeActionsUseTimeAfterRowLock(t *testing.T) {
	for _, action := range []string{"save_notice_draft", "publish_notice", "archive_notice"} {
		t.Run(action, func(t *testing.T) {
			a, db := noticeTestService(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var id int64
			if err := db.pool.QueryRow(ctx, `INSERT INTO admin_notices(title,targets,channels,created_by) VALUES('原始草稿','{all}','{site}','legacy-token') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM admin_notices WHERE id=$1 FOR UPDATE`, id); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"action":%q,"id":%q}`, action, fmt.Sprint(id))
			if action == "save_notice_draft" {
				body = fmt.Sprintf(`{"action":%q,"id":%q,"value":{"title":"更新后的草稿","targets":["all"],"channels":["site"]}}`, action, fmt.Sprint(id))
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest(http.MethodPost, "/api/actions", strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				a.AdminHTTP(w, r.WithContext(adminTestContext(ctx, "legacy-token")))
				done <- w
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())

			// 等待中的操作必须使用这次已提交更新之后的时间，不能把公告版本时间写回事务开始时。
			var marker time.Time
			if err := blocker.QueryRow(ctx, `UPDATE admin_notices SET title='并发保存的草稿',updated_at=clock_timestamp() WHERE id=$1 RETURNING updated_at`, id).Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if action == "publish_notice" {
				// 新发布的公告应排在等待期间发布的公告之前，而不是被公开列表的上限挤掉。
				if _, err := blocker.Exec(ctx, `INSERT INTO admin_notices(title,targets,channels,status,created_by,published_at) SELECT '等待期间发布的公告','{all}','{site}','live','legacy-token',clock_timestamp() FROM generate_series(1,$1)`, noticePublicLimit); err != nil {
					t.Fatal(err)
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			w := <-done
			if w.Code != http.StatusOK {
				t.Fatal("公告操作失败", w.Code, w.Body.String())
			}
			var updated time.Time
			var published *time.Time
			if err := db.pool.QueryRow(ctx, `SELECT updated_at,published_at FROM admin_notices WHERE id=$1`, id).Scan(&updated, &published); err != nil {
				t.Fatal(err)
			}
			if updated.Before(marker) {
				t.Errorf("公告更新时间倒退：更新后 %s，锁等待期间已保存 %s", updated, marker)
			}
			if action == "publish_notice" {
				if published == nil || published.Before(marker) || !published.Equal(updated) {
					t.Errorf("公告发布时间未与锁后的更新时间一致：发布 %v，更新 %s，锁等待期间已保存 %s", published, updated, marker)
				}
				items := publicNotices(t, a, NoticesPath)
				if len(items) != noticePublicLimit || items[0]["id"] != fmt.Sprint(id) {
					t.Error("刚完成发布的公告没有排在公开列表首位", items)
				}
			}
		})
	}
}
