package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// The histogram estimates percentiles inside the bucket that holds them and survives the database's JSON form.
func TestLatencyHistogram(t *testing.T) {
	var c metricCounts
	if _, ok := c.latency.percentile(0.95); ok {
		t.Fatal("an empty histogram has a percentile")
	}
	for range 19 {
		c.observe(80*time.Millisecond, false, 0)
	}
	c.observe(4*time.Second, true, 10)
	if c.calls != 20 || c.errors != 1 || c.usage != 0 {
		t.Fatalf("counts = %+v", c)
	}
	if p95, _ := c.latency.percentile(0.95); p95 < 50 || p95 > 100 {
		t.Fatalf("p95 = %d, want inside the 50..100ms bucket", p95)
	}
	if p100, _ := c.latency.percentile(1); p100 <= 3000 || p100 > 5000 {
		t.Fatalf("max = %d, want inside the 3000..5000ms bucket", p100)
	}
	c.observe(10*time.Minute, false, 2.5)
	if c.usage != 2.5 {
		t.Fatalf("usage of a successful call = %v", c.usage)
	}
	if p, _ := c.latency.percentile(1); p != 120000 {
		t.Fatalf("overflow percentile = %d", p)
	}
	encoded := c.latency.histogramJSON()
	if encoded["100"] != 19 || encoded["5000"] != 1 || encoded[overflowBucket] != 1 || len(encoded) != 3 {
		t.Fatalf("histogram JSON = %v", encoded)
	}
	if histogramFromJSON(encoded) != c.latency {
		t.Fatal("histogram JSON does not round-trip")
	}
	// A bound the code no longer uses lands in the next bucket up rather than being lost.
	if h := histogramFromJSON(map[string]int64{"60": 2, "bogus": 1, "10": -1}); h[2] != 2 || h[len(latencyBoundsMS)] != 1 {
		t.Fatalf("foreign keys = %v", h)
	}
}

// pendingCalls sums service's unflushed minute buckets.
func pendingCalls(m *serviceMetrics, service string) metricCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total metricCounts
	for key, c := range m.minutes {
		if key.service == service {
			total.add(*c)
		}
	}
	return total
}

// 批次同时提取小时与分钟桶，顺序固定；重试裁剪过期行，新调用保留在下一批。
func TestServiceMetricsBuckets(t *testing.T) {
	var m serviceMetrics
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	m.record("chat", now.Add(-7*time.Minute), time.Second, false, 0)
	m.record("chat", now.Add(-2*time.Minute), time.Second, true, 0)
	m.record("chat", now, 2*time.Second, false, 0)
	m.record("translation", now, time.Millisecond, false, 12)
	if w := pendingCalls(&m, "chat"); w.calls != 3 || w.errors != 1 {
		t.Fatalf("pending chat minutes = %+v", w)
	}
	m.record("cloud", now.Add(-8*24*time.Hour), time.Second, false, 0)
	batch := m.nextBatch(now)
	minutes := batch.Minutes
	if len(minutes) != 4 {
		t.Fatalf("minutes = %+v", minutes)
	}
	for i, want := range []string{"chat 12:23", "chat 12:28", "chat 12:30", "translation 12:30"} {
		if got := minutes[i].Service + " " + minutes[i].Minute.Format("15:04"); got != want || minutes[i].Latency == nil {
			t.Fatalf("minute %d = %s %+v, want %s", i, got, minutes[i], want)
		}
	}
	if minutes[1].Errors != 1 || minutes[3].Calls != 1 {
		t.Fatalf("minute counts = %+v", minutes)
	}
	if pending := m.pending(); len(pending) != 2 {
		t.Fatalf("pending = %+v", pending)
	}
	rows := batch.Hours
	if len(rows) != 2 {
		t.Fatalf("take = %+v", rows)
	}
	for _, row := range rows {
		if !row.Hour.Equal(now.Truncate(time.Hour)) || (row.Service == "chat" && (row.Calls != 3 || row.Errors != 1)) || (row.Service == "translation" && row.Usage != 12) {
			t.Fatalf("row = %+v", row)
		}
	}
	m.record("chat", now, time.Second, false, 0)
	restored := map[string]int64{}
	for _, row := range m.pending() {
		restored[row.Service] += row.Calls
	}
	if restored["chat"] != 4 || restored["translation"] != 1 || restored["cloud"] != 0 {
		t.Fatalf("restored = %v", restored)
	}
	if retried := m.nextBatch(now.Add(2 * time.Hour)); retried.ID != batch.ID || len(retried.Minutes) != 0 || retried.Hours[0].Calls != 3 {
		t.Fatalf("重试混入新调用或保留了过期分钟：%+v", retried)
	}
	if expired := m.nextBatch(now.Add(8 * 24 * time.Hour)); expired != nil || len(m.pending()) != 0 {
		t.Fatalf("过期批次或积压桶没有清理：%+v", expired)
	}
}

func TestJudgeService(t *testing.T) {
	window := func(calls, errors int, latency time.Duration) metricCounts {
		var c metricCounts
		for i := range calls {
			c.observe(latency, i < errors, 0)
		}
		return c
	}
	for _, tc := range []struct {
		name string
		w    metricCounts
		want string
	}{
		{"no traffic", metricCounts{}, stateIdle},
		{"healthy", window(50, 1, 100*time.Millisecond), stateOK},
		{"one failure of two", window(2, 1, 100*time.Millisecond), stateOK},
		{"error rate", window(20, 2, 100*time.Millisecond), stateDegraded},
		{"slow", window(5, 0, 4*time.Second), stateDegraded},
		{"slow but too few calls", window(2, 0, 4*time.Second), stateOK},
		{"all failing", window(3, 3, 100*time.Millisecond), stateDown},
	} {
		if got := judgeService(tc.w, 3000); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestQuotaUnits(t *testing.T) {
	month := metricCounts{calls: 10, usage: 7200}
	if meteredUnits(meterSeconds, month) != 2 || meteredUnits(meterChars, month) != 7200 || meteredUnits(meterCalls, month) != 10 {
		t.Fatal("metered units")
	}
	for _, tc := range []struct {
		unit, meter string
		want        float64
	}{
		{"calls", meterSeconds, 10}, {"hours", meterSeconds, 2}, {"chars", meterChars, 7200}, {"chars", meterCalls, 0}, {"hours", meterCalls, 0}, {"cny", meterCalls, 3.5},
	} {
		if got := quotaUsed(tc.unit, tc.meter, month, 3.5); got != tc.want {
			t.Errorf("%s/%s = %v, want %v", tc.unit, tc.meter, got, tc.want)
		}
	}
	if s := wavSeconds(testWAV()); math.Abs(s-4.0/32000) > 1e-12 {
		t.Fatalf("wav seconds = %v", s)
	}
	if wavSeconds([]byte("RIFF")) != 0 {
		t.Fatal("a truncated WAV has a duration")
	}
	if formatLatency(310) != "310ms" || formatLatency(1840) != "1.8s" {
		t.Fatal("latency format")
	}
}

// Calls made for a tagged request are recorded with their outcome and metered usage, never their content; nothing is recorded while the console is disabled.
func TestUpstreamCallsAreMetered(t *testing.T) {
	failing, rejecting := false, false
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if failing {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/" && r.Method == "GET" {
			if rejecting {
				_, _ = io.WriteString(w, `["FAILED_TO_PARSE_REQUEST_BODY"]`)
				return
			}
			_, _ = io.WriteString(w, `["SUCCESS",[["q",["你好"]]]]`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "messages") {
			if rejecting {
				_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"测试"}}]}`)
			return
		}
		if rejecting {
			_, _ = io.WriteString(w, `{"code":500,"data":""}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":200,"data":"test"}`)
	})
	if w := call(s, "POST", "/v1/translate", `{"text":"测试一下","source_lang":"auto","target_lang":"en"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if rows := s.metrics.pending(); len(rows) != 0 {
		t.Fatalf("recorded while the console is disabled: %+v", rows)
	}
	s.config.Admin.Enabled = true
	if w := call(s, "POST", "/v1/translate", `{"text":"测试一下","source_lang":"auto","target_lang":"en"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"secret prompt"}]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "GET", "/v1/cloud/candidates?text=nihao", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	failing = true
	if w := call(s, "POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"secret prompt"}]}`); w.Code != 502 {
		t.Fatal(w.Code, w.Body)
	}
	// An upstream that answers 200 with an error body failed too, and a failed translation meters no characters.
	failing, rejecting = false, true
	if w := call(s, "POST", "/v1/translate", `{"text":"测试一下","source_lang":"auto","target_lang":"en"}`); w.Code != 502 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "GET", "/v1/cloud/candidates?text=nihao", ""); w.Code != 502 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"secret prompt"}]}`); w.Code != 502 {
		t.Fatal(w.Code, w.Body)
	}
	// A request refused before any upstream call is not a call.
	if w := call(s, "POST", "/v1/translate", `{"text":"x","source_lang":"??","target_lang":"en"}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	got := map[string]account.ServiceMetric{}
	for _, row := range s.metrics.pending() {
		got[row.Service] = row
	}
	if got["translation"].Calls != 2 || got["translation"].Usage != 4 || got["translation"].Errors != 1 {
		t.Fatalf("translation = %+v", got["translation"])
	}
	if got["chat"].Calls != 3 || got["chat"].Errors != 2 || got["chat"].Usage != 0 {
		t.Fatalf("chat = %+v", got["chat"])
	}
	if got["cloud"].Calls != 2 || got["cloud"].Errors != 1 || len(got) != 3 {
		t.Fatalf("services = %+v", got)
	}
	encoded, _ := json.Marshal(s.metrics.pending())
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "测试") {
		t.Fatalf("metrics hold content: %s", encoded)
	}
	// A call the client abandoned says nothing about the upstream.
	s.observe("chat", time.Now(), context.Canceled, 0)
	s.observe("chat", time.Now(), errors.Join(errors.New("dial"), context.Canceled), 0)
	if w := pendingCalls(&s.metrics, "chat"); w.calls != 3 {
		t.Fatalf("cancelled calls were recorded: %+v", w)
	}
}

// Without admin.services the pages list the configured upstreams under default names; with it, exactly the configured services.
func TestMonitoredServices(t *testing.T) {
	s := fixture(t, nil)
	var keys []string
	for _, svc := range s.monitoredServices() {
		keys = append(keys, svc.Key)
		// Transcribing a whole recording is judged by a longer threshold than the 3s default.
		wantSlow := defaultServiceSlowMS
		if svc.Key == "transcription" {
			wantSlow = 10000
		}
		if svc.Name == "" || svc.Provider != "127.0.0.1" || svc.SlowMS != wantSlow {
			t.Fatalf("derived service = %+v", svc)
		}
	}
	if strings.Join(keys, ",") != "cloud,chat,translation,transcription" {
		t.Fatalf("derived keys = %v", keys)
	}
	// The account side receives the derived list for the overview, without it counting as configured.
	if settings := s.adminAccountSettings(); len(settings.Services) != 0 || len(settings.DerivedServices) != 4 || settings.DerivedServices[1].Key != "chat" || settings.DerivedServices[1].Name != "AI 联想" || settings.DerivedServices[1].Provider != "127.0.0.1" {
		t.Fatalf("account settings = %+v", settings)
	}
	s.config.Translation.Provider = "tencent"
	if svc := s.monitoredServices()[2]; svc.Provider != "腾讯 TMT" {
		t.Fatalf("translation provider = %+v", svc)
	}
	s.config.Admin.Services = []AdminServiceConfig{{Key: "chat", Name: "AI 联想", Provider: "账号通道", Quota: AdminServiceQuota{Limit: 2000, Unit: "cny", UnitPrice: 0.05}, SlowMS: 2000}}
	services := s.monitoredServices()
	if len(services) != 1 || services[0] != (monitoredService{Key: "chat", Name: "AI 联想", Provider: "账号通道", SlowMS: 2000, QuotaLimit: 2000, QuotaUnit: "cny", UnitPrice: 0.05}) {
		t.Fatalf("configured services = %+v", services)
	}
	if settings := s.adminAccountSettings(); len(settings.Services) != 1 || len(settings.DerivedServices) != 0 {
		t.Fatalf("configured account settings = %+v", settings)
	}
	// "database" is the status probe's own key: a configured service under it would mix its metrics with the probe's pings.
	s.config.Admin.Services = append(s.config.Admin.Services, AdminServiceConfig{Key: databaseService, Name: "数据库", Provider: "x"})
	if services = s.monitoredServices(); len(services) != 1 || services[0].Key != "chat" {
		t.Fatalf("configured database service = %+v", services)
	}
	if status := s.statusServices(); len(status) != 2 || status[0].Key != databaseService || status[0].Provider != "PostgreSQL" || status[1].Key != "chat" {
		t.Fatalf("status services = %+v", status)
	}
}

// /api/cloud needs view_cloud_usage even though it is a read, and both pages are GET only.
func TestStatusAndCloudAccess(t *testing.T) {
	s, _ := adminRBACFixture(t)
	reviewer := strings.Repeat("r", 64)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", "/api/cloud", reviewer, ""))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "permission_denied") {
		t.Fatalf("reviewer cloud: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/cloud", "/api/status"} {
		w = httptest.NewRecorder()
		r := adminRequest(s, "POST", path, "", strings.Repeat("a", 40))
		s.ServeHTTP(w, r)
		if w.Code != 405 {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body)
		}
	}
	if got := s.adminHealth(context.Background()); got != stateUnknown {
		t.Fatalf("health before any probe = %s", got)
	}
}

func monitoringServer(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	disposableSchema(t)
	return monitoringReplica(t, upstream)
}

func manualMonitoringServer(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	admin, schema := disposableSchema(t)
	s := monitoringReplica(t, upstream)
	// 手动探针从空序列开始，避免回拨到当天时与启动探针占用同一分钟。
	if _, err := admin.Exec(t.Context(), "TRUNCATE "+pgx.Identifier{schema, "admin_service_verdicts"}.Sanitize()+", "+pgx.Identifier{schema, "admin_service_daily"}.Sanitize()); err != nil {
		t.Fatalf("清理启动探针状态：%v", err)
	}
	return s
}

func TestStatusProbeSequenceStartsInStartupMinute(t *testing.T) {
	s := manualMonitoringServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"测试"}}]}`)
	})
	ctx := t.Context()
	// 首个手动探针与真实启动探针使用同一分钟，覆盖午夜前回拨测试时间的情况。
	now := time.Now().Truncate(time.Minute).Add(-time.Minute)
	for range 4 {
		s.metrics.record("chat", now, 4*time.Second, false, 0)
	}
	for i := 1; i <= incidentOpenAfter; i++ {
		s.statusTick(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	incidents, err := s.accounts.Incidents(ctx, 5)
	if err != nil || len(incidents) != 1 || incidents[0].Service != "chat" || incidents[0].State != "open" || !incidents[0].Auto {
		t.Fatalf("启动分钟打断了手动探针序列：%+v，错误：%v", incidents, err)
	}
}

// monitoringReplica starts a server on the database schema of the running test, so a second call is another replica of the first.
func monitoringReplica(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	t.Setenv("TEST_CLIENT_TOKEN", testToken)
	t.Setenv("TEST_ADMIN_TOKEN", strings.Repeat("q", 48))
	t.Setenv("TEST_UPSTREAM_TOKEN", "provider-secret")
	srv := httptest.NewTLSServer(upstream)
	t.Cleanup(srv.Close)
	s, err := New(Config{
		Auth:    account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"},
		Admin:   AdminConfig{Enabled: true, Host: "admin.example.com", TokenEnv: "TEST_ADMIN_TOKEN", Services: []AdminServiceConfig{{Key: "chat", Name: "AI 联想", Provider: "账号通道", Quota: AdminServiceQuota{Limit: 100, Unit: "calls", UnitPrice: 0.5}, SlowMS: 2000}, {Key: "translation", Name: "在线翻译", Provider: "腾讯 TMT"}}},
		Clients: []Client{{ID: "device", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 120}},
		Chat:    ChatEndpoint{Endpoint: Endpoint{URL: srv.URL, TokenEnv: "TEST_UPSTREAM_TOKEN", Model: "m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.client = srv.Client()
	t.Cleanup(func() {
		s.Close()
		s.CloseAccounts()
	})
	// statusProbeJob runs its first probe at startup; wait for it so the test's probes do not interleave with it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if checked, _, err := s.statusStates(context.Background()); err == nil && !checked.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the startup probe did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s
}

func adminGet(t *testing.T, s *Server, path string, out any) {
	t.Helper()
	r := httptest.NewRequest("GET", "https://admin.example.com"+path, nil)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("q", 48))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}

type statusResponse struct {
	CheckedAt *time.Time `json:"checked_at"`
	State     string     `json:"state"`
	Services  []struct {
		Key       string   `json:"key"`
		Name      string   `json:"name"`
		Desc      string   `json:"desc"`
		State     string   `json:"state"`
		P95MS     *int     `json:"p95_ms"`
		Uptime60d *float64 `json:"uptime_60d"`
		Days      []struct {
			Day    string   `json:"day"`
			State  string   `json:"state"`
			Uptime *float64 `json:"uptime"`
		} `json:"days"`
	} `json:"services"`
	Incidents []struct {
		ID          int64  `json:"id"`
		Service     string `json:"service"`
		ServiceName string `json:"service_name"`
		Title       string `json:"title"`
		State       string `json:"state"`
		Auto        bool   `json:"auto"`
	} `json:"incidents"`
}

// sameUTCDay returns now, unless probes at the next ticks minutes after it would cross UTC midnight, in which case it returns a time just early enough that they all land on now's UTC day. Verdicts roll into the daily table by their minute's UTC date while the status page's today is the real clock's, so a test run in the last minutes of a day would otherwise find its verdicts on tomorrow's row. The last probe still lies within statusStale of the real clock, so the page reports it as current.
func sameUTCDay(now time.Time, ticks int) time.Time {
	end := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if now.Add(time.Duration(ticks) * time.Minute).Before(end) {
		return now
	}
	return end.Add(-time.Duration(ticks)*time.Minute - time.Second)
}

// End to end against PostgreSQL: slow upstream calls degrade the service, three bad probes open one automatic incident, the pages report the minute rollup and the month's quota, and five good probes resolve the incident.
func TestStatusProbeAndPages(t *testing.T) {
	s := manualMonitoringServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"测试"}}]}`)
	})
	ctx := context.Background()
	// One real proxied call, then slow calls recorded directly: the window is degraded by P95.
	if w := call(s, "POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	now := sameUTCDay(time.Now(), incidentOpenAfter)
	for range 4 {
		s.metrics.record("chat", now, 4*time.Second, false, 0)
	}
	// Each probe judges the five complete minutes before its own, so the next three minutes all see the slow calls.
	for i := 1; i <= incidentOpenAfter; i++ {
		s.statusTick(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	if got := s.adminHealth(ctx); got != stateDegraded {
		t.Fatalf("health = %s", got)
	}
	var status statusResponse
	adminGet(t, s, "/api/status", &status)
	if status.CheckedAt == nil || status.State != stateDegraded || len(status.Services) != 3 {
		t.Fatalf("status = %+v", status)
	}
	db, chat, translation := status.Services[0], status.Services[1], status.Services[2]
	if db.Key != databaseService || db.State != stateOK || db.Desc != "PostgreSQL" || db.P95MS == nil {
		t.Fatalf("database = %+v", db)
	}
	if chat.Key != "chat" || chat.Name != "AI 联想" || chat.Desc != "账号通道" || chat.State != stateDegraded || chat.P95MS == nil || *chat.P95MS <= 2000 || len(chat.Days) != statusDays {
		t.Fatalf("chat = %+v", chat)
	}
	todayStrip := chat.Days[statusDays-1]
	if todayStrip.Day != time.Now().UTC().Format(time.DateOnly) || todayStrip.State != stateDegraded || todayStrip.Uptime == nil || *todayStrip.Uptime != 1 || chat.Uptime60d == nil || chat.Days[0].State != "none" {
		t.Fatalf("chat strip today = %+v, uptime %v", todayStrip, chat.Uptime60d)
	}
	if translation.State != stateIdle || translation.P95MS != nil {
		t.Fatalf("idle translation = %+v", translation)
	}
	if len(status.Incidents) != 1 || status.Incidents[0].Title != "AI 联想响应变慢" || status.Incidents[0].ServiceName != "AI 联想" || status.Incidents[0].State != "open" || !status.Incidents[0].Auto {
		t.Fatalf("incidents = %+v", status.Incidents)
	}

	var cloud struct {
		Services []struct {
			Key       string   `json:"key"`
			State     string   `json:"state"`
			Calls     int64    `json:"calls_24h"`
			ErrorRate *float64 `json:"error_rate"`
			P95MS     *int     `json:"p95_ms"`
			Hourly    []struct {
				Hour  time.Time `json:"hour"`
				Calls int64     `json:"calls"`
			} `json:"hourly"`
			Month struct {
				Calls int64  `json:"calls"`
				Meter string `json:"meter"`
			} `json:"month"`
			Quota *struct {
				Limit float64 `json:"limit"`
				Unit  string  `json:"unit"`
				Used  float64 `json:"used"`
				Pct   float64 `json:"pct"`
			} `json:"quota"`
			CostCNY *float64 `json:"cost_cny"`
		} `json:"services"`
	}
	// One more call stays in memory until the next flush; the page includes it.
	s.metrics.record("chat", time.Now(), time.Second, true, 0)
	adminGet(t, s, "/api/cloud", &cloud)
	if len(cloud.Services) != 2 {
		t.Fatalf("cloud = %+v", cloud)
	}
	c := cloud.Services[0]
	if c.Key != "chat" || c.State != stateDegraded || c.Calls != 6 || c.ErrorRate == nil || math.Abs(*c.ErrorRate-1.0/6) > 1e-9 || c.P95MS == nil || len(c.Hourly) != cloudHours || c.Hourly[cloudHours-1].Calls != 6 {
		t.Fatalf("chat card = %+v", c)
	}
	if c.Month.Calls != 6 || c.Month.Meter != meterCalls || c.Quota == nil || c.Quota.Used != 6 || c.Quota.Pct != 6 || c.Quota.Unit != "calls" || c.CostCNY == nil || *c.CostCNY != 3 {
		t.Fatalf("chat month = %+v quota %+v cost %v", c.Month, c.Quota, c.CostCNY)
	}
	if tr := cloud.Services[1]; tr.Calls != 0 || tr.ErrorRate != nil || tr.Quota != nil || tr.CostCNY != nil || tr.Month.Meter != meterChars {
		t.Fatalf("idle translation card = %+v", tr)
	}

	// Recovery: the window is empty from ten minutes on, and the fifth good probe resolves the incident.
	later := now.Add(10 * time.Minute)
	for i := range incidentResolveAfter {
		s.statusTick(ctx, later.Add(time.Duration(i)*time.Minute))
		incidents, err := s.accounts.Incidents(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		if resolved := incidents[0].State == "resolved"; resolved != (i == incidentResolveAfter-1) {
			t.Fatalf("after %d good probes the incident is %s", i+1, incidents[0].State)
		}
	}
	if got := s.adminHealth(ctx); got != stateOK {
		t.Fatalf("health after recovery = %s", got)
	}
}

// statusReplicas starts two replicas sharing one database, the first of which leads the status probe, and returns an administrative connection to their schema.
func statusReplicas(t *testing.T) (a, b *Server, admin *pgx.Conn) {
	t.Helper()
	admin, schema := disposableSchema(t)
	if _, err := admin.Exec(context.Background(), "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	ok := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"测试"}}]}`)
	}
	a = monitoringReplica(t, ok)
	b = monitoringReplica(t, ok)
	if !leads(a) || leads(b) {
		t.Fatalf("leaders: a %v, b %v", leads(a), leads(b))
	}
	return a, b, admin
}

func leads(s *Server) bool {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.statusLeader != nil
}

// latestVerdict is the stored verdict of service in the newest judged minute.
func latestVerdict(t *testing.T, s *Server, service string) account.ServiceVerdict {
	t.Helper()
	latest, err := s.accounts.LatestServiceVerdicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range latest {
		if v.Service == service {
			return v
		}
	}
	t.Fatalf("no verdict for %s in %+v", service, latest)
	return account.ServiceVerdict{}
}

// autoIncidents lists the states of the automatic incidents of service, oldest first.
func autoIncidents(t *testing.T, s *Server, service string) []string {
	t.Helper()
	incidents, err := s.accounts.Incidents(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for i := len(incidents) - 1; i >= 0; i-- {
		if incidents[i].Auto && incidents[i].Service == service {
			states = append(states, incidents[i].State)
		}
	}
	return states
}

// With two replicas only the leader judges, it judges the calls of both, each minute reaches the daily table once, and both replicas report the same status.
func TestStatusProbeJudgesAllReplicasOnce(t *testing.T) {
	a, b, admin := statusReplicas(t)
	ctx := context.Background()
	now := time.Now()
	// Two slow calls on each replica: neither alone has the three calls a P95 verdict needs, together they are degraded.
	for _, s := range []*Server{a, b} {
		for range 2 {
			s.metrics.record("chat", now, 4*time.Second, false, 0)
		}
	}
	for i := 1; i <= incidentOpenAfter; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		b.statusTick(ctx, at)
		a.statusTick(ctx, at)
		b.statusTick(ctx, at)
		// A repeated judgement of the same minute, as from a leader that lost its session mid-minute and its successor, changes nothing.
		a.statusTick(ctx, at)
	}
	if leads(b) {
		t.Fatal("the second replica took the lead while the first held it")
	}
	chat := latestVerdict(t, b, "chat")
	if chat.State != stateDegraded || chat.Calls != 4 || chat.BadRuns != incidentOpenAfter || !chat.Incident {
		t.Fatalf("chat verdict = %+v", chat)
	}
	if got := autoIncidents(t, a, "chat"); len(got) != 1 || got[0] != "open" {
		t.Fatalf("automatic incidents = %v", got)
	}
	for _, s := range []*Server{a, b} {
		if got := s.adminHealth(ctx); got != stateDegraded {
			t.Fatalf("health = %s", got)
		}
		var status statusResponse
		adminGet(t, s, "/api/status", &status)
		if status.State != stateDegraded || status.Services[1].Key != "chat" || status.Services[1].State != stateDegraded {
			t.Fatalf("status = %+v", status)
		}
	}
	// Every judged minute counts once in the daily table, however many replicas probed it. The schema is this test's own, so every row is compared; summing over all days keeps the check true when the probes cross UTC midnight.
	var minutes, total int
	if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_service_verdicts WHERE service='chat'),(SELECT coalesce(sum(total_minutes),0) FROM admin_service_daily WHERE service='chat')`).Scan(&minutes, &total); err != nil {
		t.Fatal(err)
	}
	if minutes < incidentOpenAfter || total != minutes {
		t.Fatalf("daily total_minutes = %d for %d judged minutes", total, minutes)
	}
}

// When the leader stops, the other replica takes over at its next probe and carries on the stored streaks: it neither opens a second incident nor resolves the open one before five good minutes, and while it does not lead, its own quiet traffic resolves nothing.
func TestStatusProbeFailover(t *testing.T) {
	a, b, admin := statusReplicas(t)
	ctx := context.Background()
	now := time.Now()
	for range 4 {
		a.metrics.record("chat", now, 4*time.Second, false, 0)
	}
	for i := 1; i <= incidentOpenAfter; i++ {
		a.statusTick(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	if got := autoIncidents(t, a, "chat"); len(got) != 1 || got[0] != "open" {
		t.Fatalf("automatic incidents = %v", got)
	}
	// The follower sees no failing calls of its own, but it does not judge, so the incident stays open.
	for i := range incidentResolveAfter + 1 {
		b.statusTick(ctx, now.Add(time.Duration(10+i)*time.Minute))
	}
	if got := autoIncidents(t, b, "chat"); len(got) != 1 || got[0] != "open" || leads(b) {
		t.Fatalf("after the follower's probes: incidents %v, b leads %v", got, leads(b))
	}
	// The leader stops; the follower takes over within one probe and continues the bad streak without a second incident.
	a.Close()
	b.statusTick(ctx, now.Add(4*time.Minute))
	if !leads(b) {
		t.Fatal("the follower did not take over")
	}
	if chat := latestVerdict(t, b, "chat"); chat.State != stateDegraded || chat.BadRuns != incidentOpenAfter+1 || !chat.Incident {
		t.Fatalf("chat verdict after failover = %+v", chat)
	}
	if got := autoIncidents(t, b, "chat"); len(got) != 1 || got[0] != "open" {
		t.Fatalf("automatic incidents after failover = %v", got)
	}
	// Recovery under the new leader resolves the incident on exactly the fifth good minute.
	for i := range incidentResolveAfter {
		b.statusTick(ctx, now.Add(time.Duration(20+i)*time.Minute))
		if got := autoIncidents(t, b, "chat"); got[0] != "open" && i < incidentResolveAfter-1 {
			t.Fatalf("resolved after %d good probes", i+1)
		}
	}
	if got := autoIncidents(t, b, "chat"); len(got) != 1 || got[0] != "resolved" {
		t.Fatalf("automatic incidents after recovery = %v", got)
	}
	// A leader whose session dies loses the lock; the next replica to probe takes it, and the old leader does not get it back while the new one holds it.
	c := monitoringReplica(t, func(w http.ResponseWriter, r *http.Request) {})
	// pg_locks is cluster-wide and other tests' replicas, or other runs against the same server, hold the same lock in their own schemas, so only this database and schema's lock is touched.
	const leaderLock = `FROM pg_locks WHERE locktype='advisory' AND granted AND classid=1836282740 AND objsubid=2 AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND objid=(hashtext(current_schema())::bigint & 4294967295)::oid`
	var pid int
	if err := admin.QueryRow(ctx, `SELECT pid `+leaderLock).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	// pg_terminate_backend only signals the backend; its lock goes when it has exited. Wait for that backend rather than for the lock to be free: c's startup probe may still be running and can take the lock the moment it is released.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var alive bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminated leader session did not exit")
		}
	}
	c.statusTick(ctx, now.Add(30*time.Minute))
	b.statusTick(ctx, now.Add(31*time.Minute))
	if !leads(c) || leads(b) {
		t.Fatalf("after the leader's session died: c leads %v, b leads %v", leads(c), leads(b))
	}
}

// An automatic incident an admin resolves while the service is still failing is not reopened by the same streak, including by a new leader; a new streak opens a new one.
func TestStatusProbeRespectsManualResolve(t *testing.T) {
	a, b, _ := statusReplicas(t)
	ctx := context.Background()
	now := time.Now()
	for range 4 {
		a.metrics.record("chat", now, 4*time.Second, false, 0)
	}
	for i := 1; i <= incidentOpenAfter; i++ {
		a.statusTick(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	if _, err := a.accounts.ResolveAutoIncident(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	a.statusTick(ctx, now.Add(4*time.Minute))
	a.Close()
	b.statusTick(ctx, now.Add(5*time.Minute))
	if got := autoIncidents(t, b, "chat"); len(got) != 1 || got[0] != "resolved" || !leads(b) {
		t.Fatalf("automatic incidents during the same streak = %v", got)
	}
	// After a good minute the next bad streak is new.
	b.statusTick(ctx, now.Add(15*time.Minute))
	later := now.Add(16 * time.Minute)
	for range 4 {
		b.metrics.record("chat", later, 4*time.Second, false, 0)
	}
	for i := 1; i <= incidentOpenAfter; i++ {
		b.statusTick(ctx, later.Add(time.Duration(i)*time.Minute))
	}
	if got := autoIncidents(t, b, "chat"); len(got) != 2 || got[1] != "open" {
		t.Fatalf("automatic incidents after a new streak = %v", got)
	}
}

// Tencent answers its errors with HTTP 200: the call counts as failed and meters no characters, while a good batch meters only the characters it sent.
func TestTencentCallsAreMetered(t *testing.T) {
	reply := `{"Response":{"Error":{"Code":"FailedOperation.NoFreeAmount","Message":"quota"}}}`
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, reply)
	})
	s.config.Admin.Enabled = true
	s.config.Translation.Provider = "tencent"
	s.config.Translation.secretID = "test-id"
	s.config.Translation.Region = "ap-guangzhou"
	if w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"auto","target_lang":"en"}`); w.Code != 502 {
		t.Fatal(w.Code, w.Body)
	}
	reply = `{"Response":{"TargetTextList":["test"]}}`
	if w := call(s, "POST", "/v1/translate", `{"text":"测试","source_lang":"auto","target_lang":"en"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	rows := s.metrics.pending()
	if len(rows) != 1 || rows[0].Service != "translation" || rows[0].Calls != 2 || rows[0].Errors != 1 || rows[0].Usage != 2 {
		t.Fatalf("translation = %+v", rows)
	}
}

// A streaming session is failed only when the upstream side broke: a client that drops its connection without a close frame is not an upstream failure.
func TestStreamingSessionsAreMetered(t *testing.T) {
	for _, tc := range []struct {
		name         string
		upstreamDrop bool
	}{{"client drops", false}, {"upstream drops", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				kind, data, err := c.Read(ctx)
				if err != nil || tc.upstreamDrop {
					return
				}
				_ = c.Write(ctx, kind, data)
				_, _, _ = c.Read(ctx)
			})
			s.config.Admin.Enabled = true
			s.config.Streaming = StreamingEndpoint{URL: strings.Replace(s.config.Cloud.URL, "https:", "wss:", 1), token: "provider-secret", ResourceID: "synthetic-resource", MaxSeconds: 5}
			live := httptest.NewTLSServer(s)
			t.Cleanup(func() { s.Close(); live.Close() })
			c := dialStream(t, live)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := c.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
				t.Fatal(err)
			}
			if tc.upstreamDrop {
				if _, _, err := c.Read(ctx); err == nil {
					t.Fatal("stream stayed open after the upstream dropped")
				}
			} else {
				if _, _, err := c.Read(ctx); err != nil {
					t.Fatal(err)
				}
				_ = c.CloseNow()
			}
			deadline := time.Now().Add(3 * time.Second)
			var window metricCounts
			for time.Now().Before(deadline) {
				if window = pendingCalls(&s.metrics, "streaming"); window.calls > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			wantErrors := int64(0)
			if tc.upstreamDrop {
				wantErrors = 1
			}
			if window.calls != 1 || window.errors != wantErrors {
				t.Fatalf("streaming = %+v", window)
			}
		})
	}
}

// Sessions that end because they reached max_seconds are not upstream failures, whichever relay direction notices the deadline first.
func TestStreamingSessionsEndingAtMaxSecondsAreNotFailures(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = c.Read(ctx)
	})
	s.config.Admin.Enabled = true
	s.config.Streaming = StreamingEndpoint{URL: strings.Replace(s.config.Cloud.URL, "https:", "wss:", 1), token: "provider-secret", ResourceID: "synthetic-resource", MaxSeconds: 1}
	live := httptest.NewTLSServer(s)
	t.Cleanup(func() { s.Close(); live.Close() })
	// Each session picks its first reporter at random, so several sessions make a misclassification all but certain to show.
	const sessions = 8
	done := make(chan struct{}, sessions)
	for range sessions {
		c := dialStream(t, live)
		go func() {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, _ = c.Read(ctx)
		}()
	}
	for range sessions {
		<-done
	}
	deadline := time.Now().Add(3 * time.Second)
	var window metricCounts
	for time.Now().Before(deadline) {
		if window = pendingCalls(&s.metrics, "streaming"); window.calls == sessions {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if window.calls != sessions || window.errors != 0 {
		t.Fatalf("streaming = %+v", window)
	}
}

// The final metrics flush waits for the streaming sessions that shutdown cancels, so a call they record while closing still reaches the database.
func TestCloseFlushesCallsRecordedByCancelledStreams(t *testing.T) {
	s := monitoringServer(t, func(w http.ResponseWriter, r *http.Request) {})
	started := time.Now()
	s.streams.Add(1)
	go func() {
		defer s.streams.Done()
		<-s.lifetime.Done()
		// A cancelled session records its call only after its close handshake.
		time.Sleep(200 * time.Millisecond)
		s.observeCall("streaming", time.Now(), time.Second, false, 3)
	}()
	s.Close()
	rows, err := s.accounts.ServiceMetrics(context.Background(), started.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	for _, row := range rows {
		if row.Service == "streaming" {
			calls += row.Calls
		}
	}
	if calls != 1 {
		t.Fatalf("streaming calls flushed = %d, rows %+v", calls, rows)
	}
}
