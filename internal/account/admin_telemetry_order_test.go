package account

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTelemetryOutOfOrderCrashWritesPreserveEventTimes(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "新分组"
		if existing {
			name = "已有分组"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			a := &Service{store: db}
			resetCrashTables(t, a)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			signature := crashSignature("ios", "合成乱序崩溃", "")
			var firstSeen time.Time
			if existing {
				if err := db.pool.QueryRow(ctx, `INSERT INTO admin_crash_groups(signature,platform,version,title,status,issue_url,first_seen,last_seen)
VALUES($1,'ios','base','保留的标题','fixed','https://example.test/issue',now()-interval '2 days',now()-interval '1 day')
RETURNING first_seen`, signature).Scan(&firstSeen); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			// 临时占用事件 ID，让较早请求在写入事件时等待；回滚后它仍能正常插入。
			if _, err := blocker.Exec(ctx, `INSERT INTO admin_events(id,kind,platform,version) VALUES('telemetry-order-early','download','ios','blocker')`); err != nil {
				t.Fatal(err)
			}
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				a.Telemetry(w, jsonRequest("POST", TelemetryPath, `{"id":"telemetry-order-early","kind":"crash","platform":"ios","version":"9.0","message":"合成乱序崩溃"}`, "").WithContext(ctx))
				result <- w
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 较晚请求先完成；版本按事件时间选择，不能按应用版本号或事务完成顺序选择。
			w := httptest.NewRecorder()
			a.Telemetry(w, jsonRequest("POST", TelemetryPath, `{"id":"telemetry-order-later","kind":"crash","platform":"ios","version":"1.0","message":"合成乱序崩溃"}`, "").WithContext(ctx))
			if w.Code != 202 {
				t.Fatal("较晚事件未成功写入", w.Code, w.Body.String())
			}
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case w := <-result:
				if w.Code != 202 {
					t.Fatal("较早事件未成功写入", w.Code, w.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("较早事件未结束", ctx.Err())
			}
			var version, title, status string
			var issueURL *string
			var first, last, earliest, latest time.Time
			if err := db.pool.QueryRow(ctx, `SELECT g.version,g.title,g.status,g.issue_url,g.first_seen,g.last_seen,min(e.created_at),max(e.created_at)
FROM admin_crash_groups g JOIN admin_events e USING(signature)
WHERE g.signature=$1 GROUP BY g.signature`, signature).Scan(&version, &title, &status, &issueURL, &first, &last, &earliest, &latest); err != nil {
				t.Fatal(err)
			}
			if !earliest.Before(latest) || version != "1.0" || !last.Equal(latest) {
				t.Errorf("分组未保留最新事件的版本和时间：version=%q last=%s earliest=%s latest=%s", version, last, earliest, latest)
			}
			if existing {
				if !first.Equal(firstSeen) || title != "保留的标题" || status != "fixed" || issueURL == nil || *issueURL != "https://example.test/issue" {
					t.Error("乱序写入改变了已有分组的首次时间、标题、状态或 Issue")
				}
			} else if !first.Equal(earliest) || status != "open" || issueURL != nil {
				t.Errorf("新分组未覆盖最早事件或初始状态错误：first=%s earliest=%s status=%q issue=%v", first, earliest, status, issueURL)
			}
		})
	}
}
