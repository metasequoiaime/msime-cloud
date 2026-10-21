package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func monitoringService(t *testing.T) (*Service, *Store) {
	t.Helper()
	db := testStore(t)
	if _, err := db.pool.Exec(context.Background(), `TRUNCATE admin_service_metrics, admin_service_daily, admin_service_minutes, admin_service_verdicts, admin_service_metric_batches, admin_incidents, admin_audit`); err != nil {
		t.Fatal(err)
	}
	return &Service{store: db}, db
}

// Hourly deltas add up per service and hour, latency buckets included, and the totals and rows read back as written.
func TestServiceMetricsAccumulate(t *testing.T) {
	a, _ := monitoringService(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour)
	first := []ServiceMetric{
		{Service: "chat", Hour: hour.Add(17 * time.Minute), Calls: 3, Errors: 1, Latency: map[string]int64{"100": 2, "inf": 1}},
		{Service: "translation", Hour: hour.Add(-time.Hour), Calls: 2, Latency: map[string]int64{"50": 2}, Usage: 40},
	}
	second := []ServiceMetric{{Service: "chat", Hour: hour, Calls: 2, Errors: 0, Latency: map[string]int64{"100": 1, "3000": 1}}}
	if err := a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Hours: first}); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Hours: second}); err != nil {
		t.Fatal(err)
	}
	rows, err := a.ServiceMetrics(ctx, hour.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	chat := rows[1]
	if chat.Service != "chat" || !chat.Hour.Equal(hour) || chat.Calls != 5 || chat.Errors != 1 {
		t.Fatalf("chat = %+v", chat)
	}
	if chat.Latency["100"] != 3 || chat.Latency["inf"] != 1 || chat.Latency["3000"] != 1 || len(chat.Latency) != 3 {
		t.Fatalf("latency buckets = %v", chat.Latency)
	}
	usage, err := a.ServiceUsageSince(ctx, hour.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if usage["translation"] != (ServiceUsage{Calls: 2, Usage: 40}) || usage["chat"].Calls != 5 {
		t.Fatalf("usage = %+v", usage)
	}
	if only, err := a.ServiceUsageSince(ctx, hour); err != nil || len(only) != 1 {
		t.Fatalf("usage since this hour = %+v, %v", only, err)
	}
	if err := a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Hours: []ServiceMetric{{Service: "Bad Key", Hour: hour, Calls: 1}}}); err == nil {
		t.Fatal("an invalid service key was stored")
	}
}

// Probe minutes roll into one row per service and day: available minutes, the degraded flag and the latest P95.
func TestServiceProbesRollUpDaily(t *testing.T) {
	a, db := monitoringService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	p95 := 420
	later := 380
	for _, probes := range [][]ServiceProbe{
		{{Service: "database", Available: true}, {Service: "chat", Available: true, P95MS: &p95}},
		{{Service: "database", Available: true}, {Service: "chat", Available: false, Degraded: true}},
		{{Service: "database", Available: true}, {Service: "chat", Available: true, P95MS: &later}},
	} {
		if err := a.RecordServiceProbes(ctx, now, probes); err != nil {
			t.Fatal(err)
		}
	}
	days, err := a.ServiceDays(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %+v", days)
	}
	for _, d := range days {
		switch d.Service {
		case "chat":
			if d.OKMinutes != 2 || d.TotalMinutes != 3 || !d.Degraded || d.P95MS == nil || *d.P95MS != 380 {
				t.Fatalf("chat day = %+v", d)
			}
		case "database":
			if d.OKMinutes != 3 || d.TotalMinutes != 3 || d.Degraded || d.P95MS != nil {
				t.Fatalf("database day = %+v", d)
			}
		}
	}
	// A full day cannot be exceeded, even by several instances rolling the same minute.
	if _, err = db.pool.Exec(ctx, `UPDATE admin_service_daily SET ok_minutes=1440,total_minutes=1440 WHERE service='database'`); err != nil {
		t.Fatal(err)
	}
	if err = a.RecordServiceProbes(ctx, now, []ServiceProbe{{Service: "database", Available: true}}); err != nil {
		t.Fatal(err)
	}
	var ok, total int
	if err = db.pool.QueryRow(ctx, `SELECT ok_minutes,total_minutes FROM admin_service_daily WHERE service='database'`).Scan(&ok, &total); err != nil || ok != 1440 || total != 1440 {
		t.Fatalf("capped day = %d/%d, %v", ok, total, err)
	}
	// An outage minute after the cap still shows on the full day.
	if err = a.RecordServiceProbes(ctx, now, []ServiceProbe{{Service: "database", Available: false, Degraded: true}}); err != nil {
		t.Fatal(err)
	}
	if err = db.pool.QueryRow(ctx, `SELECT ok_minutes,total_minutes FROM admin_service_daily WHERE service='database'`).Scan(&ok, &total); err != nil || ok != 1439 || total != 1440 {
		t.Fatalf("outage on a full day = %d/%d, %v", ok, total, err)
	}
}

// Retention keeps 60 days of daily rows and 90 days of hourly metrics.
func TestPruneServiceMonitoring(t *testing.T) {
	a, db := monitoringService(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_service_daily(service,day,ok_minutes,total_minutes) VALUES
 ('chat',(now() AT TIME ZONE 'UTC')::date-59,1,1),('chat',(now() AT TIME ZONE 'UTC')::date-60,1,1);
INSERT INTO admin_service_metrics(service,hour,calls) VALUES('chat',date_trunc('hour',now())-interval '89 days',1),('chat',date_trunc('hour',now())-interval '91 days',1)`); err != nil {
		t.Fatal(err)
	}
	if err := a.PruneServiceMonitoring(ctx); err != nil {
		t.Fatal(err)
	}
	var days, hours int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_service_daily),(SELECT count(*) FROM admin_service_metrics)`).Scan(&days, &hours); err != nil {
		t.Fatal(err)
	}
	if days != 1 || hours != 1 {
		t.Fatalf("after pruning: %d daily rows, %d hourly rows", days, hours)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_service_minutes(minute,service,calls) VALUES(date_trunc('minute',now())-interval '170 minutes','chat',1),(date_trunc('minute',now())-interval '190 minutes','chat',1);
INSERT INTO admin_service_verdicts(minute,service,state,checked_at) VALUES(date_trunc('minute',now())-interval '170 minutes','chat','ok',now()),(date_trunc('minute',now())-interval '190 minutes','chat','ok',now())`); err != nil {
		t.Fatal(err)
	}
	if err := a.PruneServiceMonitoring(ctx); err != nil {
		t.Fatal(err)
	}
	var minutes, verdicts int
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_service_minutes),(SELECT count(*) FROM admin_service_verdicts)`).Scan(&minutes, &verdicts); err != nil {
		t.Fatal(err)
	}
	if minutes != 1 || verdicts != 1 {
		t.Fatalf("after pruning: %d minute rows, %d verdicts", minutes, verdicts)
	}
	if err := a.PingDatabase(ctx); err != nil {
		t.Fatal(err)
	}
}

// The probe opens at most one automatic incident per service and resolves only its own.
func TestAutoIncidentLifecycle(t *testing.T) {
	a, db := monitoringService(t)
	ctx := context.Background()
	since := lastNotificationID(t, db)
	opened, err := a.OpenAutoIncident(ctx, "chat", "AI 联想响应变慢", "最近 5 分钟 P95 4.2s")
	if err != nil || !opened {
		t.Fatalf("open = %v, %v", opened, err)
	}
	if opened, err = a.OpenAutoIncident(ctx, "chat", "AI 联想响应变慢", ""); err != nil || opened {
		t.Fatalf("second open = %v, %v", opened, err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO admin_incidents(service,title) VALUES('chat','人工记录')`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.OpenAutoIncident(ctx, "chat", "", ""); err == nil {
		t.Fatal("an empty title was accepted")
	}
	resolved, err := a.ResolveAutoIncident(ctx, "chat")
	if err != nil || !resolved {
		t.Fatalf("resolve = %v, %v", resolved, err)
	}
	if resolved, err = a.ResolveAutoIncident(ctx, "chat"); err != nil || resolved {
		t.Fatalf("second resolve = %v, %v", resolved, err)
	}
	incidents, err := a.Incidents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 2 || incidents[0].Title != "人工记录" || incidents[0].State != "open" || incidents[0].Auto || incidents[1].State != "resolved" || incidents[1].ResolvedAt == nil || !incidents[1].Auto {
		t.Fatalf("incidents = %+v", incidents)
	}
	// Only the automatic incident that actually opened is announced, pointing at the status page.
	if got := notificationTargets(t, db, NotifyIncident, since); len(got) != 1 || !strings.HasPrefix(got[0], "status ") {
		t.Fatal("incident notifications", got)
	}
}

func incidentAction(a *Service, ctx context.Context, body string) *httptest.ResponseRecorder {
	r := jsonRequest("POST", "/api/actions", body, "")
	w := httptest.NewRecorder()
	a.AdminHTTP(w, r.WithContext(ctx))
	return w
}

// The incident actions validate their input, need triage_issues, and are audited with the change in the same transaction.
func TestIncidentActions(t *testing.T) {
	a, db := monitoringService(t)
	owner := adminTestContext(context.Background(), "google:x:admin@example.test")
	w := incidentAction(a, owner, `{"action":"open_incident","value":{"service":"translation","title":"翻译间歇超时","description":"上游限流"}}`)
	if w.Code != 200 {
		t.Fatalf("open: %d %s", w.Code, w.Body)
	}
	var opened struct {
		OK       bool  `json:"ok"`
		Affected int64 `json:"affected"`
		ID       int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opened); err != nil || !opened.OK || opened.Affected != 1 || opened.ID < 1 {
		t.Fatalf("open response %s", w.Body)
	}
	id := jsonString(opened.ID)
	if w = incidentAction(a, owner, `{"action":"update_incident","id":"`+id+`","value":{"description":"上游限流，已切换备用通道"},"reason":"补充处理"}`); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if w = incidentAction(a, owner, `{"action":"resolve_incident","id":"`+id+`","reason":"恢复"}`); w.Code != 200 {
		t.Fatalf("resolve: %d %s", w.Code, w.Body)
	}
	var state, description string
	var auto bool
	if err := db.pool.QueryRow(context.Background(), `SELECT state,description,auto FROM admin_incidents WHERE id=$1`, opened.ID).Scan(&state, &description, &auto); err != nil || state != "resolved" || description != "上游限流，已切换备用通道" || auto {
		t.Fatalf("incident = %s %q %v, %v", state, description, auto, err)
	}
	rows, err := db.pool.Query(context.Background(), `SELECT action,target,actor,detail::text FROM admin_audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var audit []string
	for rows.Next() {
		var action, target, actor, detail string
		if err = rows.Scan(&action, &target, &actor, &detail); err != nil {
			t.Fatal(err)
		}
		if target != id || actor != "google:x:admin@example.test" {
			t.Fatalf("audit target/actor = %s %s", target, actor)
		}
		audit = append(audit, action+" "+detail)
	}
	if len(audit) != 3 || !strings.Contains(audit[0], `"title": "翻译间歇超时"`) || !strings.Contains(audit[1], `"fields": ["description"]`) || !strings.Contains(audit[1], `"reason": "补充处理"`) || !strings.Contains(audit[2], `"reason": "恢复"`) {
		t.Fatalf("audit = %q", audit)
	}

	for _, tc := range []struct {
		body string
		code int
		err  string
	}{
		{`{"action":"resolve_incident","id":"` + id + `"}`, 409, "already_resolved"},
		{`{"action":"resolve_incident","id":"999999"}`, 404, "not_found"},
		{`{"action":"resolve_incident","id":"abc"}`, 400, "invalid_id"},
		{`{"action":"resolve_incident"}`, 400, "invalid_id"},
		{`{"action":"update_incident","id":"999999","value":{"title":"x"}}`, 404, "not_found"},
		{`{"action":"update_incident","id":"` + id + `","value":{}}`, 400, "invalid_value"},
		{`{"action":"update_incident","id":"` + id + `","value":{"service":"chat"}}`, 400, "invalid_value"},
		{`{"action":"update_incident","id":"` + id + `","value":{"title":""}}`, 400, "invalid_title"},
		{`{"action":"open_incident","value":{"service":"Bad Key","title":"x"}}`, 400, "invalid_service"},
		{`{"action":"open_incident","value":{"service":"chat"}}`, 400, "invalid_title"},
		{`{"action":"open_incident","value":{"service":"chat","title":"x","extra":1}}`, 400, "invalid_value"},
		{`{"action":"open_incident","value":{"service":"chat","title":"x","description":"` + strings.Repeat("a", 5001) + `"}}`, 400, "invalid_description"},
		{`{"action":"open_incident"}`, 400, "invalid_value"},
	} {
		w = incidentAction(a, owner, tc.body)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.err) {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body)
		}
	}

	// A role without triage_issues is refused before anything is written.
	readonly := WithAdminAccess(context.Background(), AdminAccess{Actor: "pat:ro@example.test", Email: "ro@example.test", Role: "readonly", Permissions: []string{PermViewCloudUsage}})
	if w = incidentAction(a, readonly, `{"action":"open_incident","value":{"service":"chat","title":"x"}}`); w.Code != 403 {
		t.Fatalf("readonly open: %d %s", w.Code, w.Body)
	}
	reviewer := WithAdminAccess(context.Background(), AdminAccess{Actor: "google:r:rev@example.test", Email: "rev@example.test", Role: "reviewer", Permissions: []string{PermTriageIssues}})
	if w = incidentAction(a, reviewer, `{"action":"open_incident","value":{"service":"chat","title":"AI 联想排队"}}`); w.Code != 200 {
		t.Fatalf("reviewer open: %d %s", w.Code, w.Body)
	}
	var incidents, audits int
	if err = db.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM admin_incidents),(SELECT count(*) FROM admin_audit)`).Scan(&incidents, &audits); err != nil || incidents != 2 || audits != 4 {
		t.Fatalf("after refusals: %d incidents, %d audit rows, %v", incidents, audits, err)
	}
	// Once the console knows which services exist, an incident can only be opened for one of them or for the database probe.
	a.ConfigureAdmin(AdminSettings{DerivedServices: []AdminService{{Key: "chat", Name: "AI 联想"}}})
	if w = incidentAction(a, owner, `{"action":"open_incident","value":{"service":"translation","title":"x"}}`); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_service") {
		t.Fatalf("unmonitored service: %d %s", w.Code, w.Body)
	}
	for _, key := range []string{"chat", "database"} {
		if w = incidentAction(a, owner, `{"action":"open_incident","value":{"service":"`+key+`","title":"x"}}`); w.Code != 200 {
			t.Fatalf("%s: %d %s", key, w.Code, w.Body)
		}
	}
	a.ConfigureAdmin(AdminSettings{Services: []AdminService{{Key: "translation"}}, DerivedServices: []AdminService{{Key: "chat"}}})
	if w = incidentAction(a, owner, `{"action":"open_incident","value":{"service":"chat","title":"x"}}`); w.Code != 400 {
		t.Fatalf("configured services replace the derived ones: %d %s", w.Code, w.Body)
	}
}

func jsonString(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// Every replica adds its own minute deltas; the stored rows are their sums, and a window read returns exactly the minutes asked for.
func TestServiceMinutesAccumulate(t *testing.T) {
	a, _ := monitoringService(t)
	ctx := context.Background()
	minute := time.Now().UTC().Truncate(time.Minute)
	replicaA := []ServiceMinute{{Service: "chat", Minute: minute.Add(-2 * time.Minute), Calls: 2, Errors: 1, Latency: map[string]int64{"100": 1, "5000": 1}}, {Service: "chat", Minute: minute.Add(-time.Minute), Calls: 1, Latency: map[string]int64{"50": 1}}}
	replicaB := []ServiceMinute{{Service: "chat", Minute: minute.Add(-2*time.Minute + 30*time.Second), Calls: 3, Latency: map[string]int64{"100": 2, "inf": 1}}, {Service: "chat", Minute: minute, Calls: 9}}
	for _, rows := range [][]ServiceMinute{replicaA, replicaB} {
		if err := a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Minutes: rows}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := a.ServiceMinutes(ctx, minute.Add(-5*time.Minute), minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[0].Minute.Equal(minute.Add(-2*time.Minute)) || rows[0].Calls != 5 || rows[0].Errors != 1 || rows[1].Calls != 1 {
		t.Fatalf("window rows = %+v", rows)
	}
	if l := rows[0].Latency; l["100"] != 3 || l["5000"] != 1 || l["inf"] != 1 || len(l) != 3 {
		t.Fatalf("summed latency = %v", l)
	}
	if err := a.RecordServiceMetricBatch(ctx, ServiceMetricBatch{ID: randomToken(), Minutes: []ServiceMinute{{Service: "Bad Key", Minute: minute, Calls: 1}}}); err == nil {
		t.Fatal("an invalid service key was stored")
	}
}

// A minute's verdict is stored once: a second write of the same minute neither replaces it nor rolls the minute into the daily table again. The newest minute is the shared status, and the previous verdicts carry the streaks.
func TestServiceVerdictsCountEachMinuteOnce(t *testing.T) {
	a, db := monitoringService(t)
	ctx := context.Background()
	minute := time.Now().UTC().Truncate(time.Minute)
	if latest, err := a.LatestServiceVerdicts(ctx); err != nil || len(latest) != 0 {
		t.Fatalf("latest before any probe = %+v, %v", latest, err)
	}
	p95 := 420
	verdict := func(state string, bad int) ServiceVerdict {
		return ServiceVerdict{Service: "chat", State: state, Calls: 7, Errors: 1, P95MS: &p95, BadRuns: bad, Incident: bad >= 3, CheckedAt: time.Now(), Daily: ServiceProbe{Available: state != "down", Degraded: state != "ok", P95MS: &p95}}
	}
	for i, state := range []string{"degraded", "down", "degraded"} {
		n, err := a.RecordServiceVerdicts(ctx, minute.Add(time.Duration(i-2)*time.Minute+10*time.Second), []ServiceVerdict{verdict(state, i+1), {Service: "database", State: "ok", CheckedAt: time.Now(), Daily: ServiceProbe{Available: true}}})
		if err != nil || n != 2 {
			t.Fatalf("minute %d recorded %d, %v", i, n, err)
		}
	}
	// Another leader judging the newest minute again changes nothing.
	if n, err := a.RecordServiceVerdicts(ctx, minute, []ServiceVerdict{verdict("ok", 0), {Service: "database", State: "down", CheckedAt: time.Now()}}); err != nil || n != 0 {
		t.Fatalf("repeated minute recorded %d, %v", n, err)
	}
	var ok, total int
	if err := db.pool.QueryRow(ctx, `SELECT ok_minutes,total_minutes FROM admin_service_daily WHERE service='chat'`).Scan(&ok, &total); err != nil || ok != 2 || total != 3 {
		t.Fatalf("chat day = %d/%d, %v", ok, total, err)
	}
	latest, err := a.LatestServiceVerdicts(ctx)
	if err != nil || len(latest) != 2 || latest[0].Service != "chat" || latest[0].State != "degraded" || latest[0].BadRuns != 3 || !latest[0].Incident || *latest[0].P95MS != 420 || latest[1].State != "ok" {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	previous, err := a.PreviousServiceVerdicts(ctx, minute.Add(-10*time.Minute), minute)
	if err != nil || len(previous) != 2 || previous["chat"].State != "down" || previous["chat"].BadRuns != 2 {
		t.Fatalf("previous = %+v, %v", previous, err)
	}
	if previous, err = a.PreviousServiceVerdicts(ctx, minute.Add(-time.Minute), minute); err != nil || len(previous) != 2 || previous["chat"].BadRuns != 2 {
		t.Fatalf("previous within a minute = %+v, %v", previous, err)
	}
	if _, err = a.RecordServiceVerdicts(ctx, minute, []ServiceVerdict{{Service: "chat", State: "broken"}}); err == nil {
		t.Fatal("an unknown state was stored")
	}
	if _, err = a.OpenAutoIncident(ctx, "chat", "AI 联想响应变慢", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO admin_incidents(service,title) VALUES('translation','人工记录')`); err != nil {
		t.Fatal(err)
	}
	if open, err := a.OpenAutoIncidents(ctx); err != nil || len(open) != 1 || !open["chat"] {
		t.Fatalf("open automatic incidents = %v, %v", open, err)
	}
}

// One session at a time holds the status leader lock; giving it up, or the session dying, lets the next replica take it.
func TestStatusLeaderLock(t *testing.T) {
	a, db := monitoringService(t)
	b := &Service{store: db}
	ctx := context.Background()
	first, err := a.AcquireStatusLeader(ctx)
	if err != nil || first == nil {
		t.Fatalf("first = %v, %v", first, err)
	}
	if second, err := b.AcquireStatusLeader(ctx); err != nil || second != nil {
		t.Fatalf("a second leader = %v, %v", second, err)
	}
	if err = first.Check(ctx); err != nil {
		t.Fatal(err)
	}
	first.Release()
	second, err := b.AcquireStatusLeader(ctx)
	if err != nil || second == nil {
		t.Fatalf("after release = %v, %v", second, err)
	}
	// A session that dies, as when the leader's host vanishes, frees the lock, and the old leader's check fails.
	// pg_locks is cluster-wide and replicas of other tests or runs against the same server hold the same lock in their own schemas, so only this database and schema's lock is touched.
	const leaderLock = `FROM pg_locks WHERE locktype='advisory' AND classid=$1 AND objsubid=2 AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND objid=(hashtext(current_schema())::bigint & 4294967295)::oid`
	if _, err = db.pool.Exec(ctx, `SELECT pg_terminate_backend(pid) `+leaderLock, int64(statusLeaderLockSpace)); err != nil {
		t.Fatal(err)
	}
	if err = second.Check(ctx); err == nil {
		t.Fatal("a terminated leader still passes its check")
	}
	// pg_terminate_backend only signals the backend; its lock goes when it has exited.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var held bool
		if err = db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 `+leaderLock+`)`, int64(statusLeaderLockSpace)).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminated session kept its lock")
		}
	}
	second.Release()
	third, err := a.AcquireStatusLeader(ctx)
	if err != nil || third == nil {
		t.Fatalf("after the session died = %v, %v", third, err)
	}
	third.Release()
	var nilLeader *StatusLeader
	nilLeader.Release()
}
