package account

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/metasequoiaime/MSIME-Backend/internal/textutil"
)

// ServiceMetricBatch 是一次冲刷的增量；失败后沿用 ID，只允许丢弃过期行，不能混入新调用。
type ServiceMetricBatch struct {
	ID      string
	Hours   []ServiceMetric
	Minutes []ServiceMinute
}

// RecordServiceMetricBatch 原子登记批次并累加小时、分钟指标；提交确认丢失后重试也只计一次。
func (a *Service) RecordServiceMetricBatch(ctx context.Context, batch ServiceMetricBatch) error {
	if len(batch.Hours) == 0 && len(batch.Minutes) == 0 {
		return nil
	}
	if !textutil.ValidHex(batch.ID, 32) {
		return errors.New("invalid service metric batch ID")
	}
	for _, row := range batch.Hours {
		if !textutil.ValidShortKey(row.Service) || row.Calls < 0 || row.Errors < 0 || row.Usage < 0 {
			return errors.New("invalid service metric")
		}
	}
	for _, row := range batch.Minutes {
		if !textutil.ValidShortKey(row.Service) || row.Calls < 0 || row.Errors < 0 {
			return errors.New("invalid service minute")
		}
	}
	if batch.Hours == nil {
		batch.Hours = []ServiceMetric{}
	}
	if batch.Minutes == nil {
		batch.Minutes = []ServiceMinute{}
	}
	hours, err := json.Marshal(batch.Hours)
	if err != nil {
		return err
	}
	minutes, err := json.Marshal(batch.Minutes)
	if err != nil {
		return err
	}
	// 两次累加都依赖新登记的批次；任何写入失败都会连同批次登记一起回滚。
	// 分钟写入等待小时 CTE 的聚合结果，保证所有副本先锁小时行、再锁分钟行。
	_, err = a.store.pool.Exec(ctx, `WITH accepted AS (
 INSERT INTO admin_service_metric_batches(id) VALUES($1) ON CONFLICT DO NOTHING RETURNING id
), hourly AS (
 INSERT INTO admin_service_metrics AS m(service,hour,calls,errors,latency_buckets,usage)
 SELECT r."Service",date_trunc('hour',r."Hour",'UTC'),r."Calls",r."Errors",COALESCE(r."Latency",'{}'::jsonb),r."Usage"
 FROM accepted CROSS JOIN jsonb_to_recordset($2::jsonb) AS r("Service" text,"Hour" timestamptz,"Calls" bigint,"Errors" bigint,"Latency" jsonb,"Usage" numeric)
 ORDER BY date_trunc('hour',r."Hour",'UTC'),r."Service"
 ON CONFLICT(service,hour) DO UPDATE SET calls=m.calls+EXCLUDED.calls,errors=m.errors+EXCLUDED.errors,usage=m.usage+EXCLUDED.usage,
 latency_buckets=`+mergeLatencyBuckets+` RETURNING 1
)
INSERT INTO admin_service_minutes AS m(minute,service,calls,errors,latency_buckets)
SELECT date_trunc('minute',r."Minute",'UTC'),r."Service",r."Calls",r."Errors",COALESCE(r."Latency",'{}'::jsonb)
FROM accepted CROSS JOIN (SELECT count(*) FROM hourly) AS flushed
CROSS JOIN jsonb_to_recordset($3::jsonb) AS r("Service" text,"Minute" timestamptz,"Calls" bigint,"Errors" bigint,"Latency" jsonb)
ORDER BY date_trunc('minute',r."Minute",'UTC'),r."Service"
ON CONFLICT(minute,service) DO UPDATE SET calls=m.calls+EXCLUDED.calls,errors=m.errors+EXCLUDED.errors,
 latency_buckets=`+mergeLatencyBuckets, batch.ID, string(hours), string(minutes))
	return err
}
