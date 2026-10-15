package account

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type notificationReadWriteTrace struct{ queries atomic.Int64 }

func (trace *notificationReadWriteTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	return ctx
}

func (*notificationReadWriteTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestNotificationsReadAllUsesOneRoundTrip(t *testing.T) {
	for _, mode := range []string{"now", "shown", "missing", "missing-empty"} {
		t.Run(mode, func(t *testing.T) {
			db := notificationTestService(t).store
			var ids [3]int64
			for i := range ids {
				if err := db.pool.QueryRow(t.Context(), `INSERT INTO admin_notifications(kind,title,created_at) VALUES('issue','合成通知',now()-make_interval(hours=>$1)) RETURNING id`, 3-i).Scan(&ids[i]); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_notification_reads(email,notification_id) VALUES('read-all@example.test',$1),('read-all@example.test',$2),('other@example.test',$1)`, ids[0], ids[1]); err != nil {
				t.Fatal(err)
			}
			body := `{"all":true}`
			wantMarkers, wantUnread := 0, 0
			switch mode {
			case "shown":
				body = fmt.Sprintf(`{"all":true,"up_to_id":"%d"}`, ids[1])
				wantUnread = 1
			case "missing":
				if _, err := db.pool.Exec(t.Context(), `INSERT INTO admin_preferences(email,read_all_before) SELECT 'read-all@example.test',created_at FROM admin_notifications WHERE id=$1`, ids[0]); err != nil {
					t.Fatal(err)
				}
				body = `{"all":true,"up_to_id":"999999999999"}`
				wantMarkers, wantUnread = 1, 1
			case "missing-empty":
				body = `{"all":true,"up_to_id":"999999999999"}`
				wantMarkers, wantUnread = 2, 1
			}
			trace := &notificationReadWriteTrace{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if err = pool.Ping(t.Context()); err != nil {
				t.Fatal(err)
			}
			trace.queries.Store(0)
			a := &Service{store: &Store{pool: pool}}
			w := httptest.NewRecorder()
			a.AdminHTTP(w, notificationRequest("POST", "/api/notifications/read", body, "read-all@example.test"))
			if w.Code != 200 {
				t.Fatalf("全部已读返回 %d", w.Code)
			}
			if queries := trace.queries.Load(); queries != 1 {
				t.Errorf("全部已读使用了 %d 次数据库请求，期望 1 次", queries)
			}
			var markers, otherMarkers int
			if err = db.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE email='read-all@example.test'),count(*) FILTER (WHERE email='other@example.test') FROM admin_notification_reads`).Scan(&markers, &otherMarkers); err != nil || markers != wantMarkers || otherMarkers != 1 {
				t.Fatalf("本人标记 = %d，其他管理员标记 = %d，错误 = %v", markers, otherMarkers, err)
			}
			if unread, err := a.UnreadNotifications(t.Context(), "read-all@example.test"); err != nil || unread != wantUnread {
				t.Fatalf("本人未读 = %d，期望 %d，错误 = %v", unread, wantUnread, err)
			}
			if unread, err := a.UnreadNotifications(t.Context(), "other@example.test"); err != nil || unread != 2 {
				t.Fatalf("其他管理员未读 = %d，期望 2，错误 = %v", unread, err)
			}
		})
	}
}
