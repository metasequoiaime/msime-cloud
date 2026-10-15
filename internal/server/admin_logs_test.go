package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// fakeLoki 模拟 Loki 的 query_range 和副本统计用的即时查询：按 start（含）、end（不含）、direction 和 limit 取行，结果按副本分组。它不解释 LogQL，测试直接检查收到的查询串。
type fakeLoki struct {
	mu      sync.Mutex
	entries []lokiEntry
	queries []url.Values
	// status 非 0 时所有请求都返回这个状态码和一段不应透传的正文。
	status int
	server *httptest.Server
}

func newFakeLoki(t *testing.T) *fakeLoki {
	t.Helper()
	f := &fakeLoki{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLoki) add(entries ...lokiEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entries...)
}

func (f *fakeLoki) seen() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

func (f *fakeLoki) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	q.Set("_path", r.URL.Path)
	f.queries = append(f.queries, q)
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, `{"secret":"loki internal detail"}`)
		return
	}
	start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
	end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
	var window []lokiEntry
	for _, e := range f.entries {
		if e.ns >= start && e.ns < end {
			window = append(window, e)
		}
	}
	switch r.URL.Path {
	case "/loki/api/v1/query":
		// 只认 sum by (pod) (count_over_time(<选择器> [Ns]))：返回 (time-N 秒, time] 内有行的副本。
		match := regexp.MustCompile(`^sum by \(pod\) \(count_over_time\((\{.*\}) \[(\d+)s\]\)\)$`).FindStringSubmatch(q.Get("query"))
		at, _ := strconv.ParseInt(q.Get("time"), 10, 64)
		if match == nil || at == 0 {
			http.Error(w, "bad query", 400)
			return
		}
		seconds, _ := strconv.ParseInt(match[2], 10, 64)
		result := []map[string]any{}
		seen := map[string]bool{}
		for _, e := range f.entries {
			if e.ns > at-seconds*int64(time.Second) && e.ns <= at && !seen[e.pod] {
				seen[e.pod] = true
				result = append(result, map[string]any{"metric": map[string]string{"pod": e.pod}, "value": []any{float64(at) / 1e9, "1"}})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	case "/loki/api/v1/query_range":
		sortLogEntries(window)
		if q.Get("direction") == "backward" {
			slices.Reverse(window)
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		window = window[:min(limit, len(window))]
		type stream struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		}
		var result []*stream
		byPod := map[string]*stream{}
		for _, e := range window {
			st := byPod[e.pod]
			if st == nil {
				st = &stream{Stream: map[string]string{"pod": e.pod, "namespace": "app", "container": "msime-backend"}}
				byPod[e.pod] = st
				result = append(result, st)
			}
			st.Values = append(st.Values, [2]string{strconv.FormatInt(e.ns, 10), e.line})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": result}})
	default:
		http.NotFound(w, r)
	}
}

// compareTS 比较两个纳秒时间戳字符串。
func compareTS(a, b string) int {
	x, _ := strconv.ParseInt(a, 10, 64)
	y, _ := strconv.ParseInt(b, 10, 64)
	return int(x - y)
}

// queries0End 返回假 Loki 收到的第一个请求的 end 参数。
func queries0End(t *testing.T, loki *fakeLoki) string {
	t.Helper()
	return loki.seen()[0].Get("end")
}

// criLine 构造 promtail 送进 Loki 的一行：CRI 前缀加上 slog 默认格式。
func criLine(at time.Time, level, message string) string {
	return fmt.Sprintf("%s stderr F %s %s %s", at.UTC().Format(time.RFC3339Nano), at.UTC().Format("2006/01/02 15:04:05"), level, message)
}

func logEntryAt(at time.Time, pod, level, message string) lokiEntry {
	return lokiEntry{ns: at.UnixNano(), pod: pod, line: criLine(at, level, message)}
}

func adminLogsFixture(t *testing.T) (*Server, *fakeLoki) {
	t.Helper()
	s, _ := adminRBACFixture(t)
	loki := newFakeLoki(t)
	s.config.Admin.Logs = AdminLogsConfig{LokiURL: loki.server.URL + "/"}
	if err := s.config.Admin.Logs.validate(); err != nil {
		t.Fatal(err)
	}
	s.loki = loki.server.Client()
	return s, loki
}

var ownerCookie = strings.Repeat("o", 64)

func TestAdminLogsConfigValidation(t *testing.T) {
	c := AdminLogsConfig{}
	if err := c.validate(); err != nil || c.enabled() {
		t.Fatal("an empty block must leave the feature off", err)
	}
	c = AdminLogsConfig{LokiURL: "http://loki.loki.svc.cluster.local:3100/"}
	if err := c.validate(); err != nil || c.LokiURL != "http://loki.loki.svc.cluster.local:3100" || c.Selector != defaultLogSelector {
		t.Fatal(c, err)
	}
	if c := (AdminLogsConfig{Selector: "not validated while off"}); c.validate() != nil || c.enabled() {
		t.Fatal("a selector without loki_url must be ignored")
	}
	for _, bad := range []AdminLogsConfig{
		{LokiURL: "ftp://loki:3100"},
		{LokiURL: "http://user:pass@loki:3100"},
		{LokiURL: "http://loki:3100?x=1"},
		{LokiURL: "http://loki:3100#frag"},
		{LokiURL: "http:///nohost"},
		{LokiURL: "http://loki:3100", Selector: `namespace="app"`},
		{LokiURL: "http://loki:3100", Selector: `{namespace="app"} |= "x"`},
		{LokiURL: "http://loki:3100", Selector: `{namespace="app"}` + "\n"},
		{LokiURL: "http://loki:3100", Selector: `{namespace=app}`},
		{LokiURL: "http://loki:3100", Selector: `{` + strings.Repeat(`a="b",`, 100) + `a="b"}`},
	} {
		if err := bad.validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	for _, good := range []string{`{namespace="app", container=~"msime-.*" }`, `{job="x",pod!="y\"z"}`} {
		c := AdminLogsConfig{LokiURL: "https://loki.example.test/prefix", Selector: good}
		if err := c.validate(); err != nil {
			t.Fatal(good, err)
		}
	}
	// 配置经由 admin 块校验。
	cfg := AdminConfig{Logs: AdminLogsConfig{LokiURL: "gopher://x"}}
	if err := cfg.validateConsole(); err == nil {
		t.Fatal("validateConsole skipped admin.logs")
	}
}

// 用户输入只能以一个字符串字面量出现在 LogQL 里，无论它包含什么。
func TestLogQLEscaping(t *testing.T) {
	selector := defaultLogSelector
	if got := (logFilter{}).logQL(selector); got != selector {
		t.Fatal(got)
	}
	inject := `x"} |= "" or {namespace="kube-system"} \ ` + "中文"
	got := logFilter{pod: "app-msime-backend-77f98747f9-4nlh4", level: "WARN", q: inject}.logQL(selector)
	want := `{namespace="app",container="msime-backend",pod="app-msime-backend-77f98747f9-4nlh4"} !~ ` + strconv.Quote(logLevelPrefix+"(?:DEBUG|INFO) ") + ` |= ` + strconv.Quote(inject)
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// 拆出搜索词的字面量，反转义后必须恰好是原始输入。
	literal, ok := strings.CutPrefix(got, `{namespace="app",container="msime-backend",pod="app-msime-backend-77f98747f9-4nlh4"} !~ `+strconv.Quote(logLevelPrefix+"(?:DEBUG|INFO) ")+" |= ")
	if !ok {
		t.Fatal(got)
	}
	if v, err := strconv.Unquote(literal); err != nil || v != inject {
		t.Fatal(literal, v, err)
	}
	if !strings.Contains(literal, `\"`) || !strings.Contains(literal, `\\`) {
		t.Fatal("quotes and backslashes must be escaped", literal)
	}
	for level, below := range map[string]string{"INFO": "DEBUG", "ERROR": "DEBUG|INFO|WARN"} {
		if q := (logFilter{level: level}).logQL(selector); !strings.Contains(q, strconv.Quote(logLevelPrefix+"(?:"+below+") ")) {
			t.Fatal(level, q)
		}
	}
}

func TestLogLineParsing(t *testing.T) {
	at := time.Date(2026, 10, 1, 14, 47, 1, 378828677, time.UTC)
	line := lokiEntry{ns: at.UnixNano() + 5, pod: "p1", line: "2026-10-01T14:47:01.378828677Z stderr F 2026/10/01 14:47:01 INFO MSIME service listening address=0.0.0.0:8080"}.JSON()
	if line != (logLineJSON{TS: strconv.FormatInt(at.UnixNano()+5, 10), Time: "2026-10-01T14:47:01.378828677Z", Pod: "p1", Stream: "stderr", Level: "INFO", Message: "MSIME service listening address=0.0.0.0:8080"}) {
		t.Fatalf("%+v", line)
	}
	for _, tc := range []struct{ raw, stream, level, message string }{
		// 没有 CRI 前缀：时间取 Loki 时间戳。
		{"2026/10/01 14:47:01 WARN slow", "", "WARN", "slow"},
		// 带微秒的 slog 时间。
		{"2026-10-01T14:47:01Z stdout F 2026/10/01 14:47:01.123456 ERROR boom", "stdout", "ERROR", "boom"},
		// 无法识别级别：保留去掉 CRI 前缀后的全文。
		{"2026-10-01T14:47:01Z stderr F panic: runtime error", "stderr", "", "panic: runtime error"},
		{"2026-10-01T14:47:01Z stderr F 2026/10/01 14:47:01 http: TLS handshake error", "stderr", "", "2026/10/01 14:47:01 http: TLS handshake error"},
		{"2026-10-01T14:47:01Z stderr F 2026/10/01 14:47:01 INFO+2 custom", "stderr", "", "2026/10/01 14:47:01 INFO+2 custom"},
		{"2026/10/01 14:47:01. INFO x", "", "", "2026/10/01 14:47:01. INFO x"},
		{"not-a-time stderr F text", "", "", "not-a-time stderr F text"},
		{"2026-10-01T14:47:01Z stdin F text", "", "", "2026-10-01T14:47:01Z stdin F text"},
		{"2026-10-01T14:47:01Z stdout X text", "", "", "2026-10-01T14:47:01Z stdout X text"},
		{"short", "", "", "short"},
	} {
		got := lokiEntry{ns: at.UnixNano(), line: tc.raw}.JSON()
		if got.Stream != tc.stream || got.Level != tc.level || got.Message != tc.message {
			t.Fatalf("%q: %+v", tc.raw, got)
		}
		if tc.stream == "" && got.Time != at.Format(time.RFC3339Nano) {
			t.Fatal(got.Time)
		}
	}
	long := lokiEntry{line: strings.Repeat("a", maxLogMessageBytes-1) + "中文"}.JSON()
	if len(long.Message) != maxLogMessageBytes-1 || strings.ContainsRune(long.Message, '�') {
		t.Fatal(len(long.Message))
	}
	if got := truncateUTF8("ok\xff", 10); got != "ok�" {
		t.Fatal(got)
	}
}

func adminLogsGet(s *Server, path, cookie, bearer string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "GET", path, cookie, bearer))
	return w
}

func TestAdminLogsRecent(t *testing.T) {
	s, loki := adminLogsFixture(t)
	now := time.Now()
	loki.add(
		logEntryAt(now.Add(-20*time.Minute), "app-msime-backend-a-1", "INFO", "too old"),
		logEntryAt(now.Add(-3*time.Minute), "app-msime-backend-a-1", "INFO", "first"),
		logEntryAt(now.Add(-2*time.Minute), "app-msime-backend-b-2", "WARN", "second"),
		logEntryAt(now.Add(-1*time.Minute), "app-msime-backend-a-1", "ERROR", "third"),
	)
	w := adminLogsGet(s, "/api/logs?since=5m&limit=2&pod=app-msime-backend-a-1&level=warn&q=th%22ird", ownerCookie, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var body struct {
		Lines     []logLineJSON `json:"lines"`
		Pods      []string      `json:"pods"`
		Truncated bool          `json:"truncated"`
		Cursor    string        `json:"cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// 最新的 2 行，按时间升序，最新的在最后。
	if len(body.Lines) != 2 || body.Lines[0].Message != "second" || body.Lines[1].Message != "third" || body.Lines[1].Level != "ERROR" || body.Lines[1].Stream != "stderr" || body.Lines[0].Pod != "app-msime-backend-b-2" {
		t.Fatalf("%+v", body.Lines)
	}
	if !body.Truncated || body.Cursor != queries0End(t, loki) || !slices.Equal(body.Pods, []string{"app-msime-backend-a-1", "app-msime-backend-b-2"}) {
		t.Fatalf("%+v", body)
	}
	queries := loki.seen()
	if len(queries) != 2 {
		t.Fatal(queries)
	}
	q := queries[0]
	wantQuery := logFilter{pod: "app-msime-backend-a-1", level: "WARN", q: `th"ird`}.logQL(defaultLogSelector)
	if q.Get("_path") != "/loki/api/v1/query_range" || q.Get("query") != wantQuery || q.Get("direction") != "backward" || q.Get("limit") != "2" {
		t.Fatal(q)
	}
	start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
	end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
	if time.Duration(end-start) != 5*time.Minute {
		t.Fatal(start, end)
	}
	// 副本列表只按配置的选择器查，不带筛选条件。
	if queries[1].Get("_path") != "/loki/api/v1/query" || queries[1].Get("query") != `sum by (pod) (count_over_time(`+defaultLogSelector+` [300s]))` || queries[1].Get("time") != q.Get("end") {
		t.Fatal(queries[1])
	}

	// 窗口内没有行时游标是窗口终点。
	w = adminLogsGet(s, "/api/logs?since=1m", ownerCookie, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"lines":[]`) || !strings.Contains(w.Body.String(), `"truncated":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAdminLogsRejectsBadParameters(t *testing.T) {
	s, loki := adminLogsFixture(t)
	for path, code := range map[string]string{
		"/api/logs?since=30s":                       "invalid_since",
		"/api/logs?since=25h":                       "invalid_since",
		"/api/logs?since=abc":                       "invalid_since",
		"/api/logs?limit=0":                         "invalid_limit",
		"/api/logs?limit=2001":                      "invalid_limit",
		"/api/logs?pod=Bad_Pod":                     "invalid_pod",
		"/api/logs?pod=" + strings.Repeat("a", 254): "invalid_pod",
		"/api/logs?level=DEBUG":                     "invalid_level",
		"/api/logs?q=" + strings.Repeat("x", 201):   "invalid_query",
		"/api/logs?q=a%0Ab":                         "invalid_query",
		"/api/logs?q=%FF":                           "invalid_query",
		"/api/logs/stream?backfill=1001":            "invalid_backfill",
		"/api/logs/stream?backfill=-1":              "invalid_backfill",
		"/api/logs/stream?since=1s":                 "invalid_since",
		"/api/logs/stream?cursor=abc":               "invalid_cursor",
		"/api/logs/stream?cursor=" + strconv.FormatInt(time.Now().Add(time.Hour).UnixNano(), 10): "invalid_cursor",
		"/api/logs/stream?level=TRACE": "invalid_level",
	} {
		w := adminLogsGet(s, path, ownerCookie, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), code) {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, adminRequest(s, "POST", "/api/logs", "", strings.Repeat("a", 40)))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	if len(loki.seen()) != 0 {
		t.Fatal("rejected requests reached Loki")
	}
}

// 只有持有 view_logs 的身份（所有者、维护者、旧版管理员密钥）能读日志；功能关闭时返回 404。
func TestAdminLogsAccess(t *testing.T) {
	s, _ := adminLogsFixture(t)
	for _, path := range []string{"/api/logs", "/api/logs/stream?backfill=0"} {
		for _, tc := range []struct {
			name, cookie, bearer string
			status               int
		}{
			{"reviewer", strings.Repeat("r", 64), "", 403},
			{"readonly token", "", account.AdminTokenPrefix + "readonly", 403},
			{"anonymous", "", "", 401},
		} {
			w := adminLogsGet(s, path, tc.cookie, tc.bearer)
			if w.Code != tc.status || (tc.status == 403 && !strings.Contains(w.Body.String(), "permission_denied")) {
				t.Fatal(path, tc.name, w.Code, w.Body.String())
			}
		}
	}
	for _, tc := range []struct{ name, cookie, bearer string }{{"owner", ownerCookie, ""}, {"legacy token", "", strings.Repeat("a", 40)}} {
		if w := adminLogsGet(s, "/api/logs", tc.cookie, tc.bearer); w.Code != 200 {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
	}
	// 维护者成员：权限来自角色。
	store := s.adminStore.(*adminMemoryStore)
	store.allowed["maintainer@example.test"] = true
	store.roles["maintainer@example.test"] = adminMemoryRole{account.RoleMaintainer, []string{account.PermViewCloudUsage, account.PermViewLogs}}
	store.tokens[account.AdminTokenPrefix+"maintainer"] = account.AdminIdentity{Subject: "m", Email: "maintainer@example.test"}
	if w := adminLogsGet(s, "/api/logs", "", account.AdminTokenPrefix+"maintainer"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	s.config.Admin.Logs = AdminLogsConfig{}
	for _, path := range []string{"/api/logs", "/api/logs/stream"} {
		w := adminLogsGet(s, path, ownerCookie, "")
		if w.Code != 404 || !strings.Contains(w.Body.String(), "logs_disabled") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestAdminLogsLokiFailure(t *testing.T) {
	s, loki := adminLogsFixture(t)
	loki.status = 500
	w := adminLogsGet(s, "/api/logs", ownerCookie, "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "logs_unavailable") || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	// 副本列表失败同样是 502。
	loki.status = 0
	s.config.Admin.Logs.LokiURL = loki.server.URL + "/missing"
	if w = adminLogsGet(s, "/api/logs", ownerCookie, ""); w.Code != 502 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`not json`, `{"status":"error"}`, `{"status":"success","data":{"resultType":"matrix"}}`, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["x","y"]]}]}}`, strings.Repeat(" ", maxLokiResponseBytes+1)} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		s.config.Admin.Logs.LokiURL = bad.URL
		if w = adminLogsGet(s, "/api/logs", ownerCookie, ""); w.Code != 502 {
			t.Fatal(body[:min(len(body), 40)], w.Code)
		}
		bad.Close()
	}
	// 连不上 Loki。
	s.config.Admin.Logs.LokiURL = "http://127.0.0.1:1"
	if w = adminLogsGet(s, "/api/logs", ownerCookie, ""); w.Code != 502 {
		t.Fatal(w.Code)
	}
	if _, err := s.lokiPods(context.Background(), time.Minute, time.Now()); err == nil || strings.Contains(err.Error(), "127.0.0.1:1/loki") {
		t.Fatal("the Loki error must not carry the request URL", err)
	}
}

func TestLogStreamLimiter(t *testing.T) {
	var l logStreamLimiter
	var releases []func()
	for i := range logStreamsPerActor {
		release, ok := l.acquire("a")
		if !ok {
			t.Fatal(i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.acquire("a"); ok {
		t.Fatal("per-actor cap not enforced")
	}
	for i := logStreamsPerActor; i < logStreamsTotal; i++ {
		release, ok := l.acquire(fmt.Sprint("actor", i))
		if !ok {
			t.Fatal(i)
		}
		releases = append(releases, release)
	}
	if _, ok := l.acquire("fresh"); ok {
		t.Fatal("total cap not enforced")
	}
	releases[0]()
	releases[0]() // 重复释放不影响计数。
	if _, ok := l.acquire("a"); !ok {
		t.Fatal("released slot not reusable")
	}
	if l.total != logStreamsTotal || l.byActor["a"] != logStreamsPerActor {
		t.Fatal(l.total, l.byActor)
	}
}

// 轮询回看并去重：同一行不会发两次，晚到但时间戳更早的行仍会补发。
func TestPollLogsDedupesAcrossPolls(t *testing.T) {
	s, loki := adminLogsFixture(t)
	base := time.Now().Add(-10 * time.Second)
	a := logEntryAt(base, "p-a", "INFO", "a")
	b := logEntryAt(base.Add(time.Second), "p-b", "INFO", "b")
	loki.add(a, b)
	tail := &logTail{query: "{x=\"y\"}", seen: map[logKey]struct{}{}, cursor: base.Add(-time.Second).UnixNano(), floor: base.Add(-time.Second).UnixNano()}
	now := base.Add(2 * time.Second).UnixNano()
	out, gap, err := s.pollLogs(context.Background(), tail, now)
	// 读完时游标推进到这次读到的位置。
	if err != nil || len(gap) != 0 || len(out) != 2 || out[0].line != a.line || tail.cursor != now {
		t.Fatal(out, gap, err, tail.cursor)
	}
	// 第二次轮询会重新取回 a、b（回看窗口内），但不再发送；晚到的 c 时间戳早于 b，仍然发送。
	c := logEntryAt(base.Add(500*time.Millisecond), "p-c", "WARN", "late")
	d := logEntryAt(base.Add(1500*time.Millisecond), "p-a", "INFO", "d")
	loki.add(c, d)
	out, _, err = s.pollLogs(context.Background(), tail, now+1)
	if err != nil || len(out) != 2 || out[0].line != c.line || out[1].line != d.line {
		t.Fatal(out, err)
	}
	out, _, err = s.pollLogs(context.Background(), tail, now+2)
	if err != nil || len(out) != 0 {
		t.Fatal(out, err)
	}
	if q := loki.seen(); q[len(q)-1].Get("direction") != "forward" || q[len(q)-1].Get("limit") != strconv.Itoa(logStreamPageSize+1) {
		t.Fatal(q[len(q)-1])
	}
	// floor 及更早的行永远不发。
	loki.add(lokiEntry{ns: tail.floor, pod: "p-a", line: "at floor"})
	if out, _, _ = s.pollLogs(context.Background(), tail, now+3); len(out) != 0 {
		t.Fatal(out)
	}
	// 回看窗口外的键被清掉。
	tail.cursor += int64(time.Minute) / 2
	if _, _, err = s.pollLogs(context.Background(), tail, tail.cursor+1); err != nil || len(tail.seen) != 0 {
		t.Fatal(len(tail.seen), err)
	}
}

// 一次轮询最多读 logStreamPages 页；读满时下一次不回看，先追进度；落后太多时跳到最新位置。
func TestPollLogsPagingAndGap(t *testing.T) {
	s, loki := adminLogsFixture(t)
	base := time.Now().Add(-30 * time.Second).UnixNano()
	total := logStreamPageSize*logStreamPages + 10
	for i := range total {
		// 每两行同一时间戳，覆盖分页边界上的同时间戳去重。
		loki.add(lokiEntry{ns: base + int64(i/2), pod: "p", line: fmt.Sprint("line ", i)})
	}
	tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: base - 1, floor: base - 1}
	now := base + int64(time.Second)
	out, gap, err := s.pollLogs(context.Background(), tail, now)
	if err != nil || len(gap) != 0 || !tail.busy || len(out) < logStreamPageSize*logStreamPages-2 || len(out) > logStreamPageSize*logStreamPages {
		t.Fatal(len(out), gap, err, tail.busy)
	}
	rest, _, err := s.pollLogs(context.Background(), tail, now)
	if err != nil || tail.busy || len(out)+len(rest) != total {
		t.Fatal(len(out), len(rest), err)
	}
	all := append(out, rest...)
	seen := map[string]bool{}
	for _, e := range all {
		if seen[e.line] {
			t.Fatal("duplicate", e.line)
		}
		seen[e.line] = true
	}
	// 游标很旧但行不多（例如安静一段时间后续传）：一次读完，不算落后。
	now = time.Now().UnixNano()
	fresh := lokiEntry{ns: now - int64(time.Second), pod: "p", line: "fresh"}
	loki.add(fresh)
	tail = &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: now - int64(10*time.Minute), floor: fresh.ns - 1}
	out, gap, err = s.pollLogs(context.Background(), tail, now)
	if err != nil || len(gap) != 0 || len(out) != 1 || out[0].line != "fresh" || tail.cursor != now {
		t.Fatal(out, gap, err)
	}

	// 上一次用满了页数且仍落后超过 logStreamMaxLag：跳到最新位置，旧行不再发送。
	tail = &logTail{query: "{}", seen: map[logKey]struct{}{{1, 1}: {}}, cursor: base - int64(time.Hour), floor: base - int64(time.Hour), busy: true}
	out, gap, err = s.pollLogs(context.Background(), tail, now)
	if err != nil || len(gap) == 0 || len(out) != 1 || out[0].line != "fresh" {
		t.Fatal(out, gap, err)
	}

	loki.status = 503
	if _, _, err = s.pollLogs(context.Background(), tail, now+1); err == nil {
		t.Fatal("loki failure not reported")
	}
}

// sseEvent 是读到的一个 Server-Sent Events 帧。
type sseEvent struct {
	id, event, data string
	comment         bool
	retry           string
}

// openLogStream 通过真实的 HTTP 连接打开日志流，把读到的帧送进返回的通道。
func openLogStream(t *testing.T, s *Server, query string, header http.Header) (*http.Response, <-chan sseEvent, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/logs/stream"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "admin.msime.app"
	req.AddCookie(&http.Cookie{Name: s.adminCookieName(false), Value: ownerCookie})
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan sseEvent, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		var ev sseEvent
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				events <- ev
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
				ev.comment = true
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			case strings.HasPrefix(line, "retry: "):
				ev.retry = line[7:]
			default:
				t.Errorf("unexpected SSE line %q", line)
			}
		}
	}()
	return resp, events, cancel
}

// nextEvent 返回下一个指定类型的事件，跳过其他事件。
func nextEvent(t *testing.T, events <-chan sseEvent, kind string) sseEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream closed before %s", kind)
			}
			if ev.event == kind || (kind == "comment" && ev.comment) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", kind)
		}
	}
}

func streamLines(t *testing.T, ev sseEvent) []logLineJSON {
	t.Helper()
	var body struct {
		Lines []logLineJSON `json:"lines"`
	}
	if err := json.Unmarshal([]byte(ev.data), &body); err != nil {
		t.Fatal(err, ev.data)
	}
	return body.Lines
}

func TestAdminLogsStream(t *testing.T) {
	s, loki := adminLogsFixture(t)
	s.logPoll = 20 * time.Millisecond
	s.logHeartbeat = 30 * time.Millisecond
	now := time.Now()
	loki.add(
		logEntryAt(now.Add(-2*time.Minute), "app-msime-backend-a-1", "INFO", "old"),
		logEntryAt(now.Add(-time.Minute), "app-msime-backend-b-2", "INFO", "backfill one"),
		logEntryAt(now.Add(-30*time.Second), "app-msime-backend-a-1", "WARN", "backfill two"),
	)
	resp, events, cancel := openLogStream(t, s, "?backfill=2&level=INFO&q=back", nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal(resp.StatusCode, resp.Header)
	}
	if ev := <-events; ev.retry != "5000" {
		t.Fatalf("%+v", ev)
	}
	ev := nextEvent(t, events, "lines")
	lines := streamLines(t, ev)
	// 事件 id 是读到的位置（回填查询的终点），不早于最后一行。
	if len(lines) != 2 || lines[0].Message != "backfill one" || lines[1].Message != "backfill two" || compareTS(ev.id, lines[1].TS) < 0 {
		t.Fatalf("%+v %+v", ev, lines)
	}
	pods := nextEvent(t, events, "pods")
	if pods.data != `{"pods":["app-msime-backend-a-1","app-msime-backend-b-2"]}` {
		t.Fatal(pods.data)
	}
	if first := loki.seen()[0]; first.Get("direction") != "backward" || first.Get("limit") != "2" || first.Get("query") != (logFilter{level: "INFO", q: "back"}).logQL(defaultLogSelector) {
		t.Fatal(first)
	}
	// 没有新行时发心跳注释。
	nextEvent(t, events, "comment")

	// fresh 在之后每次轮询的回看窗口里都会被重新取回，只能发一次；late 的时间戳早于已经读到的位置，但仍在回看窗口内，也要补发。
	fresh := logEntryAt(time.Now(), "app-msime-backend-b-2", "ERROR", "fresh")
	late := logEntryAt(time.Now().Add(-2*time.Second), "app-msime-backend-b-2", "INFO", "late")
	loki.add(fresh, late)
	got := map[string]int{}
	deadline := time.After(5 * time.Second)
	for got["fresh"] == 0 || got["late"] == 0 {
		select {
		case ev := <-events:
			if ev.event == "lines" {
				for _, line := range streamLines(t, ev) {
					got[line.Message]++
				}
			}
		case <-deadline:
			t.Fatal(got)
		}
	}
	// 再等几轮轮询，确认没有重复。
	time.Sleep(10 * s.logPoll)
	for len(events) > 0 {
		if ev := <-events; ev.event == "lines" {
			for _, line := range streamLines(t, ev) {
				got[line.Message]++
			}
		}
	}
	if got["fresh"] != 1 || got["late"] != 1 || got["backfill two"] != 0 || got["old"] != 0 {
		t.Fatal(got)
	}
	s.logStreams.mu.Lock()
	open := s.logStreams.total
	s.logStreams.mu.Unlock()
	if open != 1 {
		t.Fatal(open)
	}
	// 客户端断开后流结束并释放名额。
	cancel()
	waitFor(t, "stream release", func() bool {
		s.logStreams.mu.Lock()
		defer s.logStreams.mu.Unlock()
		return s.logStreams.total == 0
	})
}

func TestAdminLogsStreamZeroBackfillStartsAtConnection(t *testing.T) {
	s, loki := adminLogsFixture(t)
	s.logPoll = 20 * time.Millisecond
	loki.add(logEntryAt(time.Now().Add(-time.Second), "p-a", "INFO", "before connection"))
	_, events, _ := openLogStream(t, s, "?backfill=0", nil)
	nextEvent(t, events, "pods")
	queries := loki.seen()
	if len(queries) == 0 || queries[0].Get("_path") != "/loki/api/v1/query" {
		t.Fatal("zero backfill must skip the historical query", queries)
	}
	connectedAt, err := strconv.ParseInt(queries[0].Get("time"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	fresh := logEntryAt(time.Now(), "p-a", "INFO", "after connection")
	loki.add(fresh)
	if lines := streamLines(t, nextEvent(t, events, "lines")); len(lines) != 1 || lines[0].Message != "after connection" {
		t.Fatalf("zero backfill returned historical lines: %+v", lines)
	}
	// 连接后的晚到日志仍在回看窗口内；时间戳相同但内容不同也应补发。
	loki.add(lokiEntry{ns: fresh.ns, pod: fresh.pod, line: "late after connection"})
	if lines := streamLines(t, nextEvent(t, events, "lines")); len(lines) != 1 || lines[0].Message != "late after connection" {
		t.Fatalf("late log was lost or duplicated: %+v", lines)
	}
	for _, query := range loki.seen() {
		if query.Get("_path") == "/loki/api/v1/query_range" {
			start, err := strconv.ParseInt(query.Get("start"), 10, 64)
			if err != nil || start < connectedAt {
				t.Fatal("zero backfill queried before the connection", query, connectedAt)
			}
		}
	}
}

func TestAdminLogsStreamResumeAndLimits(t *testing.T) {
	s, loki := adminLogsFixture(t)
	s.logPoll = 20 * time.Millisecond
	now := time.Now()
	seen := logEntryAt(now.Add(-20*time.Second), "p-a", "INFO", "already shown")
	next := logEntryAt(now.Add(-10*time.Second), "p-a", "INFO", "after cursor")
	loki.add(seen, next)
	// 带 Last-Event-ID 续传：不回填，只发游标之后的行。
	_, events, cancel := openLogStream(t, s, "", http.Header{"Last-Event-Id": {strconv.FormatInt(seen.ns, 10)}})
	ev := nextEvent(t, events, "lines")
	if lines := streamLines(t, ev); len(lines) != 1 || lines[0].Message != "after cursor" {
		t.Fatal(lines)
	}
	if q := loki.seen(); q[0].Get("_path") != "/loki/api/v1/query" {
		t.Fatal("a resumed stream must not backfill", q[0])
	}

	// 同一管理员最多同时 logStreamsPerActor 个。
	_, _, cancel2 := openLogStream(t, s, "?backfill=0", nil)
	waitFor(t, "second stream", func() bool {
		s.logStreams.mu.Lock()
		defer s.logStreams.mu.Unlock()
		return s.logStreams.total == 2
	})
	w := adminLogsGet(s, "/api/logs/stream", ownerCookie, "")
	if w.Code != 429 || w.Header().Get("Retry-After") != "30" || !strings.Contains(w.Body.String(), "too_many_streams") {
		t.Fatal(w.Code, w.Body.String())
	}
	// 另一个管理员不受影响，但走到总量上限前都能打开。
	if release, ok := s.logStreams.acquire("legacy-token"); !ok {
		t.Fatal("another admin was limited by the owner's streams")
	} else {
		release()
	}
	cancel2()

	// 优雅关闭时所有日志流结束。
	s.EndLogStreams()
	waitFor(t, "shutdown", func() bool {
		select {
		case _, ok := <-events:
			return !ok
		default:
			return false
		}
	})
	cancel()
	s.EndLogStreams() // 重复调用安全。
}

func TestAdminLogsStreamLokiErrorsAndEnd(t *testing.T) {
	s, loki := adminLogsFixture(t)
	s.logPoll = 10 * time.Millisecond
	loki.status = 503
	_, events, _ := openLogStream(t, s, "", nil)
	ev := nextEvent(t, events, "error")
	if ev.data != `{"code":"logs_unavailable"}` {
		t.Fatal(ev.data)
	}
	// 连续失败 logStreamMaxFailures 次后关闭连接，error 事件只发一次。
	errorsSeen := 1
	for ev := range events {
		if ev.event == "error" {
			errorsSeen++
		}
	}
	if errorsSeen != 1 {
		t.Fatal(errorsSeen)
	}

	// 连接满最长时间后发送 end 事件并关闭。
	loki.status = 0
	s.logMaxDuration = 50 * time.Millisecond
	_, events, _ = openLogStream(t, s, "?backfill=0", nil)
	if ev := nextEvent(t, events, "end"); ev.data != `{"reason":"max_duration"}` {
		t.Fatal(ev.data)
	}
	for range events {
	}
}
