package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMonitoringWritesUseOneDatabaseCall(t *testing.T) {
	for _, kind := range []string{"metrics", "minutes", "verdicts", "incident"} {
		t.Run(kind, func(t *testing.T) {
			_, db := monitoringService(t)
			ctx, now := t.Context(), time.Now().UTC()
			if _, err := db.pool.Exec(ctx, `TRUNCATE admin_notification_reads,admin_notifications`); err != nil {
				t.Fatal(err)
			}
			trace := &statementCancellation{}
			cfg := db.pool.Config()
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			if err := pool.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			a := &Service{store: &Store{pool: pool}}
			for attempt := range 2 {
				trace.mu.Lock()
				trace.seen, trace.statements = 0, nil
				trace.mu.Unlock()
				switch kind {
				case "metrics":
					err = a.RecordServiceMetrics(ctx, []ServiceMetric{{Service: "chat", Hour: now, Calls: 1}, {Service: "translation", Hour: now, Calls: 2}})
				case "minutes":
					err = a.RecordServiceMinutes(ctx, []ServiceMinute{{Service: "chat", Minute: now, Calls: 1}, {Service: "translation", Minute: now, Calls: 2}})
				case "verdicts":
					var recorded int
					recorded, err = a.RecordServiceVerdicts(ctx, now, []ServiceVerdict{{Service: "chat", State: "ok", CheckedAt: now}, {Service: "translation", State: "down", CheckedAt: now}})
					if recorded != 2*(1-attempt) {
						t.Fatal("监控判定去重计数错误", recorded, err)
					}
				case "incident":
					var opened bool
					opened, err = a.OpenAutoIncident(ctx, "chat", "合成监控事件", "测试")
					if opened != (attempt == 0) {
						t.Fatal("自动事件去重结果错误", opened, err)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				trace.mu.Lock()
				calls := trace.seen
				trace.mu.Unlock()
				if calls != 1 {
					t.Errorf("第 %d 次监控写入用了 %d 次数据库调用，期望 1 次", attempt+1, calls)
				}
			}
		})
	}
}

func TestMonitoringBatchWriteFailuresRollBackEarlierChanges(t *testing.T) {
	for _, kind := range []string{"metrics", "minutes", "verdicts"} {
		t.Run(kind, func(t *testing.T) {
			a, db := monitoringService(t)
			ctx, now := t.Context(), time.Now().UTC()
			table := "admin_service_" + kind
			if kind == "verdicts" {
				table = "admin_service_daily"
			}
			if _, err := db.pool.Exec(ctx, `ALTER TABLE `+table+` ADD CONSTRAINT monitoring_test_reject CHECK(service<>'translation') NOT VALID`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := db.pool.Exec(context.Background(), `ALTER TABLE `+table+` DROP CONSTRAINT IF EXISTS monitoring_test_reject`); err != nil {
					t.Error(err)
				}
			})
			var err error
			switch kind {
			case "metrics":
				err = a.RecordServiceMetrics(ctx, []ServiceMetric{{Service: "chat", Hour: now, Calls: 1}, {Service: "translation", Hour: now, Calls: 2}})
			case "minutes":
				err = a.RecordServiceMinutes(ctx, []ServiceMinute{{Service: "chat", Minute: now, Calls: 1}, {Service: "translation", Minute: now, Calls: 2}})
			case "verdicts":
				var recorded int
				recorded, err = a.RecordServiceVerdicts(ctx, now, []ServiceVerdict{{Service: "chat", State: "ok", CheckedAt: now}, {Service: "translation", State: "down", CheckedAt: now}})
				if recorded != 0 {
					t.Error("失败批次仍报告已记录的判定", recorded)
				}
			}
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" {
				t.Fatal("监控批次未返回约束错误", err)
			}
			if count := clipboardTestCount(t, db, `SELECT count(*) FROM `+table); count != 0 {
				t.Fatal("后续写入失败仍保留了批次中的先前写入", count)
			}
			if kind == "verdicts" {
				if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_service_verdicts`); count != 0 {
					t.Fatal("日聚合失败仍保留了判定", count)
				}
			}
		})
	}
}

func TestOpenAutoIncidentRollsBackWhenNotificationFails(t *testing.T) {
	a, db := monitoringService(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_notification_reads,admin_notifications`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_notifications ADD CONSTRAINT monitoring_notification_test_reject CHECK(kind<>'incident') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_notifications DROP CONSTRAINT IF EXISTS monitoring_notification_test_reject`); err != nil {
			t.Error(err)
		}
	})
	opened, err := a.OpenAutoIncident(ctx, "chat", "合成监控事件", "测试")
	var failure *pgconn.PgError
	if opened || !errors.As(err, &failure) || failure.Code != "23514" {
		t.Fatal("通知写入失败未正确返回", opened, err)
	}
	if count := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM admin_incidents)+(SELECT count(*) FROM admin_notifications)`); count != 0 {
		t.Fatal("通知失败仍保留了事件或通知", count)
	}
}
