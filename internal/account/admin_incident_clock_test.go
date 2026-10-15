package account

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIncidentResolutionTimeStartsAfterRowLockWait(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%t", automatic), func(t *testing.T) {
			a, db := monitoringService(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var id int64
			if err := db.pool.QueryRow(ctx, `INSERT INTO admin_incidents(service,title,auto) VALUES('chat','等待恢复的事件',$1) RETURNING id`, automatic).Scan(&id); err != nil {
				t.Fatal(err)
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM admin_incidents WHERE id=$1 FOR UPDATE`, id); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if automatic {
					resolved, err := a.ResolveAutoIncident(ctx, "chat")
					if err == nil && !resolved {
						err = fmt.Errorf("自动解决没有命中事件")
					}
					done <- err
					return
				}
				tx, err := db.pool.Begin(ctx)
				if err != nil {
					done <- err
					return
				}
				defer tx.Rollback(context.Background())
				_, err = actionResolveIncident(a, ctx, tx, actionRequest{ID: fmt.Sprint(id)})
				if err == nil {
					err = tx.Commit(ctx)
				}
				done <- err
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			// 只持锁而不更新行，确保实时时钟不能在 UPDATE 尝试获取行锁之前就被求值。
			var marker time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&marker); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			incidents, err := a.Incidents(ctx, 10)
			if err != nil || len(incidents) != 1 {
				t.Fatalf("读取解决后的事件失败：%+v，%v", incidents, err)
			}
			incident := incidents[0]
			if incident.State != "resolved" || incident.ResolvedAt == nil || incident.ResolvedAt.Before(marker) {
				t.Fatalf("解决时间早于行锁释放，故障持续时长被低估：事件=%+v，解锁前时间=%s", incident, marker)
			}
		})
	}
}
