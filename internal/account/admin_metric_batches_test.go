package account

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

func metricBatch(now time.Time) ServiceMetricBatch {
	return ServiceMetricBatch{ID: randomToken(),
		Hours:   []ServiceMetric{{Service: "chat", Hour: now, Calls: 3, Errors: 1, Latency: map[string]int64{"100": 3}, Usage: 4}},
		Minutes: []ServiceMinute{{Service: "chat", Minute: now, Calls: 3, Errors: 1, Latency: map[string]int64{"100": 3}}},
	}
}

func TestServiceMetricBatchConcurrentDeduplication(t *testing.T) {
	a, db := monitoringService(t)
	ctx, now := t.Context(), time.Now().UTC()
	batch := metricBatch(now)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := a.RecordServiceMetricBatch(ctx, batch); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for attempt := range 2 {
		if attempt == 1 {
			batch.ID = randomToken()
			if err := a.RecordServiceMetricBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}
		want := int64(3 * (attempt + 1))
		hours, err := a.ServiceMetrics(ctx, now.Truncate(time.Hour))
		if err != nil || len(hours) != 1 || hours[0].Calls != want || hours[0].Usage != float64(4*(attempt+1)) || hours[0].Latency["100"] != want {
			t.Fatalf("小时指标去重或累加错误：%+v，%v", hours, err)
		}
		minutes, err := a.ServiceMinutes(ctx, now.Truncate(time.Minute), now.Add(time.Minute))
		if err != nil || len(minutes) != 1 || minutes[0].Calls != want || minutes[0].Latency["100"] != want || !minutes[0].Minute.Equal(now.Truncate(time.Minute)) {
			t.Fatalf("分钟指标去重、截断或累加错误：%+v，%v", minutes, err)
		}
		if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_service_metric_batches`); count != attempt+1 {
			t.Fatal("批次登记错误", count)
		}
	}
}

func TestServiceMetricBatchFailureRollsBackRegistrationAndBothBuckets(t *testing.T) {
	a, db := monitoringService(t)
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_service_minutes ADD CONSTRAINT metric_batch_test_reject CHECK(service<>'chat') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := db.pool.Exec(context.Background(), `ALTER TABLE admin_service_minutes DROP CONSTRAINT IF EXISTS metric_batch_test_reject`)
		if err != nil {
			t.Error(err)
		}
	})
	batch := metricBatch(time.Now())
	if err := a.RecordServiceMetricBatch(ctx, batch); err == nil {
		t.Fatal("分钟约束失败仍报告成功")
	}
	if count := clipboardTestCount(t, db, `SELECT (SELECT count(*) FROM admin_service_metrics)+(SELECT count(*) FROM admin_service_minutes)+(SELECT count(*) FROM admin_service_metric_batches)`); count != 0 {
		t.Fatal("失败批次仍保留部分写入", count)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE admin_service_minutes DROP CONSTRAINT metric_batch_test_reject`); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordServiceMetricBatch(ctx, batch); err != nil {
		t.Fatal("回滚后无法重用原标识", err)
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_service_metric_batches`); count != 1 {
		t.Fatal("重试没有登记批次", count)
	}
}

func TestServiceMetricBatchValidation(t *testing.T) {
	a, db := monitoringService(t)
	if err := a.RecordServiceMetricBatch(t.Context(), ServiceMetricBatch{}); err != nil {
		t.Fatal("空批次不应访问数据库", err)
	}
	for _, mutate := range []func(*ServiceMetricBatch){
		func(b *ServiceMetricBatch) { b.ID = "not-hex" },
		func(b *ServiceMetricBatch) { b.ID = "00" },
		func(b *ServiceMetricBatch) { b.Hours[0].Service = "Bad Key" },
		func(b *ServiceMetricBatch) { b.Hours[0].Calls = -1 },
		func(b *ServiceMetricBatch) { b.Hours[0].Errors = -1 },
		func(b *ServiceMetricBatch) { b.Hours[0].Usage = -1 },
		func(b *ServiceMetricBatch) { b.Hours[0].Usage = math.NaN() },
		func(b *ServiceMetricBatch) { b.Minutes[0].Service = "Bad Key" },
		func(b *ServiceMetricBatch) { b.Minutes[0].Calls = -1 },
		func(b *ServiceMetricBatch) { b.Minutes[0].Errors = -1 },
		func(b *ServiceMetricBatch) { b.Minutes[0].Minute = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		batch := metricBatch(time.Now())
		mutate(&batch)
		if err := a.RecordServiceMetricBatch(t.Context(), batch); err == nil {
			t.Fatal("非法批次被接受", batch)
		}
	}
	if count := clipboardTestCount(t, db, `SELECT count(*) FROM admin_service_metric_batches`); count != 0 {
		t.Fatal("非法批次写入了去重记录", count)
	}
}

func TestMetricBatchReadinessAndRetention(t *testing.T) {
	a, db := monitoringService(t)
	ctx := t.Context()
	for _, drop := range []string{`DROP TABLE admin_service_metric_batches`, `DROP INDEX admin_service_metric_batches_time`} {
		if _, err := db.pool.Exec(ctx, drop); err != nil {
			t.Fatal(err)
		}
		if err := a.adminTables(ctx); err == nil {
			t.Fatal("没有发现缺失的批次对象", drop)
		}
		if err := a.AdminReady(ctx); err != nil {
			t.Fatal("未自动补齐批次对象", err)
		}
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_service_metric_batches(id,created_at) VALUES('recent',now()-interval '7 days'),('expired',now()-interval '9 days')`); err != nil {
		t.Fatal(err)
	}
	if err := a.PruneServiceMonitoring(ctx); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.pool.QueryRow(ctx, `SELECT id FROM admin_service_metric_batches`).Scan(&id); err != nil || id != "recent" {
		t.Fatal("去重记录保留期错误", id, err)
	}
}
