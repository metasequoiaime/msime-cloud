package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Upstream service monitoring storage (unit U10): metric buckets, daily availability and incidents. The collectors and the /api/status and /api/cloud handlers are in the server package.

// serviceKeyPattern is the admin.services key format; "database" is the status page's own probe.
var serviceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Retention of the monitoring tables: the status page shows 60 days, the cloud page the current month and the last 24 hours.
const (
	serviceDailyRetentionDays   = 60
	serviceMetricsRetentionDays = 90
	// serviceDetailRetention is how long the per-minute metrics and verdicts are kept: the probe reads only its window and the previous verdict, the rest is for diagnosis.
	serviceDetailRetention = 3 * time.Hour
)

// ServiceMetric is one service's upstream calls in one UTC hour. It never carries request or response content.
type ServiceMetric struct {
	Service string
	Hour    time.Time
	Calls   int64
	Errors  int64
	// Latency counts calls per latency bucket, keyed by the bucket's upper bound in milliseconds ("inf" for the overflow bucket).
	Latency map[string]int64
	// Usage is the metered quantity of the successful calls: characters or seconds, or 0 for services metered by calls.
	Usage float64
}

// RecordServiceMetrics adds the given hourly deltas to admin_service_metrics in one transaction, summing counts and latency buckets into existing rows.
func (a *Service) RecordServiceMetrics(ctx context.Context, rows []ServiceMetric) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, row := range rows {
		if !serviceKeyPattern.MatchString(row.Service) || row.Calls < 0 || row.Errors < 0 || row.Usage < 0 {
			return errors.New("invalid service metric")
		}
		latency, err := latencyJSON(row.Latency)
		if err != nil {
			return err
		}
		batch.Queue(`INSERT INTO admin_service_metrics AS m(service,hour,calls,errors,latency_buckets,usage) VALUES($1,date_trunc('hour',$2::timestamptz,'UTC'),$3,$4,$5::jsonb,$6)
ON CONFLICT(service,hour) DO UPDATE SET calls=m.calls+EXCLUDED.calls, errors=m.errors+EXCLUDED.errors, usage=m.usage+EXCLUDED.usage,
 latency_buckets=`+mergeLatencyBuckets,
			row.Service, row.Hour.UTC(), row.Calls, row.Errors, latency, row.Usage)
	}
	return a.sendBatchTx(ctx, batch)
}

// mergeLatencyBuckets sums the latency histogram of an upsert's existing row m and its EXCLUDED row, bucket by bucket.
const mergeLatencyBuckets = `(SELECT COALESCE(jsonb_object_agg(k,COALESCE((m.latency_buckets->>k)::bigint,0)+COALESCE((EXCLUDED.latency_buckets->>k)::bigint,0)),'{}'::jsonb)
  FROM (SELECT jsonb_object_keys(m.latency_buckets) UNION SELECT jsonb_object_keys(EXCLUDED.latency_buckets)) AS keys(k))`

// sendBatchTx 用批次的隐式事务原子执行全部语句，关闭结果时返回执行或提交错误。
func (a *Service) sendBatchTx(ctx context.Context, batch *pgx.Batch) error {
	return a.store.pool.SendBatch(ctx, batch).Close()
}

// latencyJSON is the jsonb form of a latency histogram; nil is the empty object.
func latencyJSON(buckets map[string]int64) (string, error) {
	if buckets == nil {
		return "{}", nil
	}
	b, err := json.Marshal(buckets)
	return string(b), err
}

// ServiceMetrics returns the hourly rows from since onwards, oldest first.
func (a *Service) ServiceMetrics(ctx context.Context, since time.Time) ([]ServiceMetric, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT service,hour,calls,errors,latency_buckets,usage::float8 FROM admin_service_metrics WHERE hour>=$1 ORDER BY hour,service`, since.UTC())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ServiceMetric, error) {
		var m ServiceMetric
		var latency []byte
		if err := row.Scan(&m.Service, &m.Hour, &m.Calls, &m.Errors, &latency, &m.Usage); err != nil {
			return m, err
		}
		m.Hour = m.Hour.UTC()
		return m, json.Unmarshal(latency, &m.Latency)
	})
}

// ServiceUsage is one service's totals over a period.
type ServiceUsage struct {
	Calls  int64
	Errors int64
	Usage  float64
}

// ServiceUsageSince sums each service's calls, errors and usage from since onwards.
func (a *Service) ServiceUsageSince(ctx context.Context, since time.Time) (map[string]ServiceUsage, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT service,sum(calls)::bigint,sum(errors)::bigint,sum(usage)::float8 FROM admin_service_metrics WHERE hour>=$1 GROUP BY service`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := map[string]ServiceUsage{}
	for rows.Next() {
		var service string
		var u ServiceUsage
		if err = rows.Scan(&service, &u.Calls, &u.Errors, &u.Usage); err != nil {
			return nil, err
		}
		usage[service] = u
	}
	return usage, rows.Err()
}

// ServiceProbe is one service's state in one probe minute.
type ServiceProbe struct {
	Service string
	// Available is false when the service was down in this minute.
	Available bool
	// Degraded marks a minute in which the service was slow or failing.
	Degraded bool
	// P95MS is the service's P95 latency so far today; nil keeps the stored value.
	P95MS *int
}

// RecordServiceProbes rolls one probe minute into admin_service_daily for the UTC day of at. Minutes are capped at a day's 1440 so several server instances cannot push a day past full; once a day is full, an unavailable minute still takes one available minute away, so an outage late in the day is not lost to the cap.
func (a *Service) RecordServiceProbes(ctx context.Context, at time.Time, probes []ServiceProbe) error {
	if len(probes) == 0 {
		return nil
	}
	day := at.UTC().Format(time.DateOnly)
	batch := &pgx.Batch{}
	for _, p := range probes {
		if !serviceKeyPattern.MatchString(p.Service) {
			return errors.New("invalid service key")
		}
		ok := 0
		if p.Available {
			ok = 1
		}
		batch.Queue(`INSERT INTO admin_service_daily AS d(service,day,ok_minutes,total_minutes,degraded,p95_ms) VALUES($1,$2::date,$3,1,$4,$5)
`+dailyRollupConflict, p.Service, day, ok, p.Degraded, p.P95MS)
	}
	return a.store.pool.SendBatch(ctx, batch).Close()
}

// dailyRollupConflict adds one probe minute, the EXCLUDED row, to an existing admin_service_daily row d.
const dailyRollupConflict = `ON CONFLICT(service,day) DO UPDATE SET total_minutes=LEAST(d.total_minutes+1,1440),
 ok_minutes=CASE WHEN d.total_minutes>=1440 THEN GREATEST(LEAST(d.ok_minutes,1440)+EXCLUDED.ok_minutes-1,0) ELSE LEAST(d.ok_minutes+EXCLUDED.ok_minutes,d.total_minutes+1) END,
 degraded=d.degraded OR EXCLUDED.degraded, p95_ms=COALESCE(EXCLUDED.p95_ms,d.p95_ms)`

// ServiceMinute is one service's upstream calls in one UTC minute: one replica's unflushed delta, or the sum of every replica once stored. It never carries request or response content.
type ServiceMinute struct {
	Service string
	Minute  time.Time
	Calls   int64
	Errors  int64
	// Latency counts calls per latency bucket, in the form of ServiceMetric.Latency.
	Latency map[string]int64
}

// RecordServiceMinutes adds the given per-minute deltas to admin_service_minutes in one transaction. Every replica flushes its own calls here, so the rows are sums over all replicas; callers pass the rows in (minute, service) order so that two replicas flushing the same minutes lock the rows in the same order and cannot deadlock.
func (a *Service) RecordServiceMinutes(ctx context.Context, rows []ServiceMinute) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, row := range rows {
		if !serviceKeyPattern.MatchString(row.Service) || row.Calls < 0 || row.Errors < 0 {
			return errors.New("invalid service minute")
		}
		latency, err := latencyJSON(row.Latency)
		if err != nil {
			return err
		}
		batch.Queue(`INSERT INTO admin_service_minutes AS m(minute,service,calls,errors,latency_buckets) VALUES(date_trunc('minute',$1::timestamptz),$2,$3,$4,$5::jsonb)
ON CONFLICT(minute,service) DO UPDATE SET calls=m.calls+EXCLUDED.calls, errors=m.errors+EXCLUDED.errors, latency_buckets=`+mergeLatencyBuckets,
			row.Minute.UTC(), row.Service, row.Calls, row.Errors, latency)
	}
	return a.sendBatchTx(ctx, batch)
}

// ServiceMinutes returns the per-minute rows of the minutes in [from, to), oldest first.
func (a *Service) ServiceMinutes(ctx context.Context, from, to time.Time) ([]ServiceMinute, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT minute,service,calls,errors,latency_buckets FROM admin_service_minutes WHERE minute>=$1 AND minute<$2 ORDER BY minute,service`, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ServiceMinute, error) {
		var m ServiceMinute
		var latency []byte
		if err := row.Scan(&m.Minute, &m.Service, &m.Calls, &m.Errors, &latency); err != nil {
			return m, err
		}
		m.Minute = m.Minute.UTC()
		return m, json.Unmarshal(latency, &m.Latency)
	})
}

// ServiceVerdict is the status probe's judgement of one service in one minute.
type ServiceVerdict struct {
	Service string
	// State is ok, idle, degraded or down.
	State string
	// Calls, Errors and P95MS describe the window the verdict was made on.
	Calls  int64
	Errors int64
	P95MS  *int
	// BadRuns and GoodRuns count consecutive degraded-or-down and ok-or-idle verdicts up to this one; Incident records that an automatic incident was opened, or found open, during the current bad streak, so it is not opened again after an admin resolves it while the service is still failing.
	BadRuns, GoodRuns int
	Incident          bool
	CheckedAt         time.Time
	// Daily is what this minute adds to admin_service_daily; its Service is ignored.
	Daily ServiceProbe
}

var verdictStates = map[string]bool{"ok": true, "idle": true, "degraded": true, "down": true}

// RecordServiceVerdicts stores the verdicts of minute and rolls each into admin_service_daily, in one transaction. A verdict already stored for the same minute and service is kept and the minute is not rolled up again, so two replicas that both believe they lead the probe still count each minute once. It reports how many verdicts were new.
func (a *Service) RecordServiceVerdicts(ctx context.Context, minute time.Time, verdicts []ServiceVerdict) (int, error) {
	if len(verdicts) == 0 {
		return 0, nil
	}
	minute = minute.UTC().Truncate(time.Minute)
	day := minute.Format(time.DateOnly)
	batch := &pgx.Batch{}
	for _, v := range verdicts {
		if !serviceKeyPattern.MatchString(v.Service) || !verdictStates[v.State] {
			return 0, errors.New("invalid service verdict")
		}
		ok := 0
		if v.Daily.Available {
			ok = 1
		}
		batch.Queue(`WITH v AS (INSERT INTO admin_service_verdicts(minute,service,state,calls,errors,p95_ms,bad_runs,good_runs,incident,checked_at) VALUES($1,$2::text,$3,$4,$5,$6,$7,$8,$9,$10)
 ON CONFLICT(minute,service) DO NOTHING RETURNING service)
INSERT INTO admin_service_daily AS d(service,day,ok_minutes,total_minutes,degraded,p95_ms) SELECT service,$11::date,$12::int,1,$13::boolean,$14::int FROM v
`+dailyRollupConflict, minute, v.Service, v.State, v.Calls, v.Errors, v.P95MS, v.BadRuns, v.GoodRuns, v.Incident, v.CheckedAt.UTC(), day, ok, v.Daily.Degraded, v.Daily.P95MS)
	}
	results := a.store.pool.SendBatch(ctx, batch)
	recorded := 0
	for range verdicts {
		tag, err := results.Exec()
		if err != nil {
			results.Close()
			return 0, err
		}
		recorded += int(tag.RowsAffected())
	}
	if err := results.Close(); err != nil {
		return 0, err
	}
	return recorded, nil
}

const verdictColumns = `service,state,calls,errors,p95_ms,bad_runs,good_runs,incident,checked_at`

func scanVerdict(row pgx.CollectableRow) (ServiceVerdict, error) {
	var v ServiceVerdict
	err := row.Scan(&v.Service, &v.State, &v.Calls, &v.Errors, &v.P95MS, &v.BadRuns, &v.GoodRuns, &v.Incident, &v.CheckedAt)
	v.CheckedAt = v.CheckedAt.UTC()
	return v, err
}

// LatestServiceVerdicts returns the verdicts of the newest judged minute, the status every replica reports, ordered by service; none before the first probe.
func (a *Service) LatestServiceVerdicts(ctx context.Context) ([]ServiceVerdict, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT `+verdictColumns+` FROM admin_service_verdicts WHERE minute=(SELECT max(minute) FROM admin_service_verdicts) ORDER BY service`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanVerdict)
}

// PreviousServiceVerdicts returns each service's newest verdict of a minute in [from, to), from which the probe continues its streaks.
func (a *Service) PreviousServiceVerdicts(ctx context.Context, from, to time.Time) (map[string]ServiceVerdict, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT DISTINCT ON (service) `+verdictColumns+` FROM admin_service_verdicts WHERE minute>=$1 AND minute<$2 ORDER BY service,minute DESC`, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanVerdict)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ServiceVerdict, len(list))
	for _, v := range list {
		out[v.Service] = v
	}
	return out, nil
}

// OpenAutoIncidents returns the services that have an automatically opened incident still open.
func (a *Service) OpenAutoIncidents(ctx context.Context) (map[string]bool, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT service FROM admin_incidents WHERE state='open' AND auto`)
	if err != nil {
		return nil, err
	}
	services, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(services))
	for _, service := range services {
		out[service] = true
	}
	return out, nil
}

// statusLeaderLockSpace is the first key of the session advisory lock held by the replica that leads the status probe; the second is hashtext(current_schema()), so deployments in separate schemas of one database each elect their own leader.
const statusLeaderLockSpace int32 = 0x6d737374

// StatusLeader is the status probe's leadership: a session advisory lock held on a connection taken out of the pool, so it lasts until Release or until the connection dies, which ends the PostgreSQL session and frees the lock for another replica. Only one goroutine may use it.
type StatusLeader struct{ conn *pgx.Conn }

// AcquireStatusLeader tries once to become the status probe's leader. It returns nil and no error while another replica leads.
func (a *Service) AcquireStatusLeader(ctx context.Context) (*StatusLeader, error) {
	pooled, err := a.store.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = pooled.QueryRow(ctx, `SELECT pg_try_advisory_lock($1,hashtext(current_schema()))`, statusLeaderLockSpace).Scan(&locked); err != nil || !locked {
		if err != nil {
			// The statement may have taken the lock before the error reached us; returned to the pool, that connection would hold the leadership with nobody judging until the pool recycled it, so end its session instead.
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = pooled.Conn().Close(closeCtx)
			cancel()
		}
		pooled.Release()
		return nil, err
	}
	// Taken out of the pool, the connection cannot be handed to a request or recycled by the pool's lifetime limits while it holds the lock.
	leader := &StatusLeader{conn: pooled.Hijack()}
	if _, err = leader.conn.Exec(ctx, serverKeepalives); err != nil {
		leader.Release()
		return nil, err
	}
	return leader, nil
}

// Check confirms that the leader's session, and with it the lock, is still alive.
func (l *StatusLeader) Check(ctx context.Context) error {
	var one int
	return l.conn.QueryRow(ctx, `SELECT 1`).Scan(&one)
}

// Release gives up leadership: it unlocks first, so the lock is free when Release returns rather than once the server has processed the disconnect, then ends the session. It is a no-op on nil.
func (l *StatusLeader) Release() {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A failed unlock leaves the lock to the end of the session below.
	_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1,hashtext(current_schema()))`, statusLeaderLockSpace)
	_ = l.conn.Close(ctx)
}

// ServiceDay is one service's availability on one UTC day.
type ServiceDay struct {
	Service      string
	Day          time.Time
	OKMinutes    int
	TotalMinutes int
	Degraded     bool
	P95MS        *int
}

// ServiceDays returns the daily rollups from the UTC day of since onwards.
func (a *Service) ServiceDays(ctx context.Context, since time.Time) ([]ServiceDay, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT service,day,ok_minutes,total_minutes,degraded,p95_ms FROM admin_service_daily WHERE day>=$1::date ORDER BY day,service`, since.UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ServiceDay, error) {
		var d ServiceDay
		err := row.Scan(&d.Service, &d.Day, &d.OKMinutes, &d.TotalMinutes, &d.Degraded, &d.P95MS)
		return d, err
	})
}

// Incident is one service incident, opened automatically by the status probe or by an admin.
type Incident struct {
	ID          int64      `json:"id"`
	Service     string     `json:"service"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"`
	StartedAt   time.Time  `json:"started_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
	Auto        bool       `json:"auto"`
}

// Incidents returns the open incidents and then the most recent resolved ones, at most limit in all.
func (a *Service) Incidents(ctx context.Context, limit int) ([]Incident, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,service,title,description,state,started_at,resolved_at,auto FROM admin_incidents ORDER BY state='open' DESC,started_at DESC,id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Incident, error) {
		var i Incident
		err := row.Scan(&i.ID, &i.Service, &i.Title, &i.Description, &i.State, &i.StartedAt, &i.ResolvedAt, &i.Auto)
		return i, err
	})
}

// OpenAutoIncident opens the probe's incident for service unless one is already open, notifying the console in the same transaction. It reports whether a new incident was opened.
func (a *Service) OpenAutoIncident(ctx context.Context, service, title, description string) (bool, error) {
	if !serviceKeyPattern.MatchString(service) || !resourceText(title, 1, 200, false) || !resourceText(description, 0, 5000, true) {
		return false, ErrInvalid
	}
	n, err := normalizeNotification(Notification{Kind: NotifyIncident, Title: title, TargetPage: "status"})
	if err != nil {
		return false, err
	}
	var id int64
	// incident 写入和通知写入共用一条数据修改 CTE，避免创建自动事件时往返两次。
	err = a.store.pool.QueryRow(ctx, `WITH incident AS (
 INSERT INTO admin_incidents(service,title,description,auto) VALUES($1,$2,$3,true)
 ON CONFLICT(service) WHERE state='open' AND auto DO NOTHING
 RETURNING id
), notified AS (
 INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at)
 SELECT $4,$5,$6,id::text,clock_timestamp() FROM incident
 RETURNING 1
)
SELECT id FROM incident`, service, title, description, n.Kind, n.Title, n.TargetPage).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ResolveAutoIncident resolves the probe's open incident for service and reports whether there was one.
func (a *Service) ResolveAutoIncident(ctx context.Context, service string) (bool, error) {
	// 先取得事件行锁，再读取解决时刻，避免 UPDATE 在等待锁前就求值而低估故障持续时间。
	tag, err := a.store.pool.Exec(ctx, `WITH locked AS MATERIALIZED (
 SELECT id FROM admin_incidents WHERE service=$1 AND state='open' AND auto FOR UPDATE
)
UPDATE admin_incidents i SET state='resolved',resolved_at=clock_timestamp()
FROM locked WHERE i.id=locked.id`, service)
	return tag.RowsAffected() > 0, err
}

// PruneServiceMonitoring drops daily rollups older than the status page's 60 days, hourly metrics older than 90 days, and per-minute metrics and verdicts older than serviceDetailRetention.
func (a *Service) PruneServiceMonitoring(ctx context.Context) error {
	detail := int(serviceDetailRetention / time.Second)
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM admin_service_daily WHERE day<(now() AT TIME ZONE 'UTC')::date-$1::int`, serviceDailyRetentionDays-1)
	batch.Queue(`DELETE FROM admin_service_metrics WHERE hour<now()-make_interval(days=>$1::int)`, serviceMetricsRetentionDays)
	batch.Queue(`DELETE FROM admin_service_minutes WHERE minute<now()-make_interval(secs=>$1::int)`, detail)
	batch.Queue(`DELETE FROM admin_service_verdicts WHERE minute<now()-make_interval(secs=>$1::int)`, detail)
	return a.store.pool.SendBatch(ctx, batch).Close()
}

// PingDatabase checks that the database answers a query, for the status probe.
func (a *Service) PingDatabase(ctx context.Context) error {
	var one int
	return a.store.pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
}

// incidentValue is the value of the incident actions; absent fields stay nil.
type incidentValue struct {
	Service     *string `json:"service"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

func decodeIncidentValue(raw json.RawMessage) (incidentValue, error) {
	var v incidentValue
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) == 0 || d.Decode(&v) != nil || d.More() {
		return v, actionFail(400, "invalid_value")
	}
	if v.Title != nil && !resourceText(*v.Title, 1, 200, false) {
		return v, actionFail(400, "invalid_title")
	}
	if v.Description != nil && !resourceText(*v.Description, 0, 5000, true) {
		return v, actionFail(400, "invalid_description")
	}
	return v, nil
}

// actionOpenIncident opens an incident from value {service, title, description}.
// monitoredService reports whether key is a service the status page shows: its own database probe, admin.services, or without them the derived upstreams. When the console knows of no service at all, any well-formed key is accepted, as before services were passed in.
func (a *Service) monitoredService(key string) bool {
	services := a.admin.Services
	if len(services) == 0 {
		services = a.admin.DerivedServices
	}
	if len(services) == 0 || key == "database" {
		return true
	}
	return slices.ContainsFunc(services, func(s AdminService) bool { return s.Key == key })
}

func actionOpenIncident(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	value, err := decodeIncidentValue(v.Value)
	if err != nil {
		return actionResult{}, err
	}
	if value.Service == nil || !serviceKeyPattern.MatchString(*value.Service) || !a.monitoredService(*value.Service) {
		return actionResult{}, actionFail(400, "invalid_service")
	}
	if value.Title == nil {
		return actionResult{}, actionFail(400, "invalid_title")
	}
	description := ""
	if value.Description != nil {
		description = *value.Description
	}
	n, err := normalizeNotification(Notification{Kind: NotifyIncident, Title: *value.Title, TargetPage: "status"})
	if err != nil {
		return actionResult{}, err
	}
	var id int64
	if err = tx.QueryRow(ctx, `WITH incident AS (
 INSERT INTO admin_incidents(service,title,description) VALUES($1,$2,$3)
 RETURNING id
), notified AS (
 INSERT INTO admin_notifications(kind,title,target_page,target_id,created_at)
 SELECT $4,$5,'status',id::text,clock_timestamp() FROM incident
 RETURNING 1
)
SELECT id FROM incident`, *value.Service, *value.Title, description, n.Kind, n.Title).Scan(&id); err != nil {
		return actionResult{}, err
	}
	target := strconv.FormatInt(id, 10)
	detail := map[string]any{"service": *value.Service, "title": *value.Title}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	return actionResult{Affected: 1, Target: target, Detail: detail, Extra: map[string]any{"id": id}}, nil
}

// actionResolveIncident 解决指定事件，锁定后的状态判断和更新共用一条语句。
func actionResolveIncident(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	id, err := numericActionID(v)
	if err != nil {
		return actionResult{}, err
	}
	var service, title string
	var updated bool
	err = tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,service,title,state FROM admin_incidents WHERE id=$1 FOR UPDATE
), updated AS (
 UPDATE admin_incidents i SET state='resolved',resolved_at=clock_timestamp()
 FROM locked WHERE i.id=locked.id AND locked.state='open'
 RETURNING i.id
)
SELECT service,title,EXISTS(SELECT 1 FROM updated) FROM locked`, id).Scan(&service, &title, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return actionResult{}, actionFail(404, "not_found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if !updated {
		return actionResult{}, actionFail(409, "already_resolved")
	}
	detail := map[string]any{"service": service, "title": title}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	return actionResult{Affected: 1, Detail: detail}, nil
}

// actionUpdateIncident changes the title or description of the incident id from value.
func actionUpdateIncident(a *Service, ctx context.Context, tx pgx.Tx, v actionRequest) (actionResult, error) {
	id, err := numericActionID(v)
	if err != nil {
		return actionResult{}, err
	}
	value, err := decodeIncidentValue(v.Value)
	if err != nil {
		return actionResult{}, err
	}
	if value.Service != nil || (value.Title == nil && value.Description == nil) {
		return actionResult{}, actionFail(400, "invalid_value")
	}
	var title string
	err = tx.QueryRow(ctx, `UPDATE admin_incidents SET title=COALESCE($2,title),description=COALESCE($3,description) WHERE id=$1 RETURNING title`, id, value.Title, value.Description).Scan(&title)
	if errors.Is(err, pgx.ErrNoRows) {
		return actionResult{}, actionFail(404, "not_found")
	}
	if err != nil {
		return actionResult{}, err
	}
	fields := []string{}
	if value.Title != nil {
		fields = append(fields, "title")
	}
	if value.Description != nil {
		fields = append(fields, "description")
	}
	detail := map[string]any{"title": title, "fields": fields}
	if v.Reason != "" {
		detail["reason"] = v.Reason
	}
	return actionResult{Affected: 1, Detail: detail}, nil
}
