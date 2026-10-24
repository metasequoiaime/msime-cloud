package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpquery"
)

// 服务日志页：从集群的 Loki 读取后端各副本的日志（`GET /api/logs` 和 `GET /api/logs/stream`）。

// AdminLogsConfig 是服务日志页读取的 Loki。loki_url 为空时功能关闭：侧栏不显示该页，两个接口都返回 404 logs_disabled。
type AdminLogsConfig struct {
	// LokiURL 是 Loki 的 HTTP 地址，例如集群内的 http://loki.loki.svc.cluster.local:3100；可以带路径前缀，不能带账号密码、查询串或片段。
	LokiURL string `json:"loki_url"`
	// Selector 是 LogQL 流选择器，选出后端各副本的日志流，默认 {namespace="app",container="msime-backend"}。这些流必须带 pod 标签。
	Selector string `json:"selector"`
}

const (
	adminLogsPath       = "/api/logs"
	adminLogsStreamPath = "/api/logs/stream"

	defaultLogSelector = `{namespace="app",container="msime-backend"}`
	// defaultLogSince 和 maxLogSince 是 GET /api/logs 与日志流回填的时间窗口。
	defaultLogSince = 15 * time.Minute
	maxLogSince     = 24 * time.Hour
	defaultLogLimit = 500
	maxLogLimit     = 2000
	// maxLogQuery 是搜索词的字节上限。
	maxLogQuery = 200
	// maxLogMessageBytes 是单行返回给前端的上限，超出部分截断。
	maxLogMessageBytes = 4096
	// maxLogTimestampBytes 覆盖带时区偏移的纳秒时间，避免解析器接受的超长小数绕过正文截断。
	maxLogTimestampBytes = len(time.RFC3339Nano)
	// maxLokiResponseBytes 限制读取的 Loki 响应体，maxLogLimit 行乘以单行上限仍在范围内。
	maxLokiResponseBytes = 16 << 20
	// maxLokiPooledResponseBytes 限制响应缓冲池的单个缓存，避免一次异常大响应长期占住 16 MiB。
	maxLokiPooledResponseBytes = 1 << 20
	// maxLogPods 是返回的副本列表上限。
	maxLogPods  = 100
	lokiTimeout = 10 * time.Second

	// 日志流每轮最多查询 logStreamPages 次、每次最多发送 logStreamPageSize 条新行；重复边界和额外一行前瞻不占发送额度。
	logStreamPoll      = 2 * time.Second
	logStreamHeartbeat = 15 * time.Second
	logStreamPageSize  = 500
	logStreamPages     = 2
	// 同一时间戳最多读取 logStreamMaxBatchSize 行；查询额外读取一行，确认超限后发送 gap，避免静默跳过或无限扩大响应。
	logStreamMaxBatchSize = 2000
	defaultLogBackfill    = 200
	maxLogBackfill        = 1000
	// logStreamOverlap 是每次轮询回看的时间：不同节点上的 promtail 推送有先后，晚到的行时间戳可能早于已经见过的最新一行，回看一段再按（时间戳、副本、行内容）去重就不会漏掉它们。
	logStreamOverlap = 5 * time.Second
	// logStreamMaxLag 是允许落后的最长时间；日志产生得比速率上限还快时，超过它就跳到最新位置并发送 gap 事件，而不是越落越远。
	logStreamMaxLag = 60 * time.Second
	// logStreamMaxCursorAge 是续传游标最早可以回到多久以前。
	logStreamMaxCursorAge = time.Hour
	// logStreamMaxDuration 是单个连接的最长时间，到期发送 end 事件后关闭，前端用游标重连。
	logStreamMaxDuration = 30 * time.Minute
	// logStreamWriteTimeout 是每次写出时延长的写超时；服务端的全局 WriteTimeout 只有几十秒，长连接必须逐次延长。
	logStreamWriteTimeout = 30 * time.Second
	// 每轮重新校验凭据与权限，鉴权存储异常时最多等待五秒后关闭流。
	logStreamAuthTimeout = 5 * time.Second
	logStreamPodsEvery   = 30 * time.Second
	logStreamMaxFailures = 5
	// 每个管理员同时最多 logStreamsPerActor 个日志流，每个副本合计最多 logStreamsTotal 个。
	logStreamsPerActor = 2
	logStreamsTotal    = 10
)

var (
	lokiResponseBufferPool sync.Pool
	logMatcher             = `\s*[A-Za-z_][A-Za-z0-9_]*\s*(?:=|!=|=~|!~)\s*"(?:[^"\\\x00-\x1f]|\\.)*"\s*`
	logSelectorPattern     = regexp.MustCompile(`^\{` + logMatcher + `(?:,` + logMatcher + `)*\}$`)
	logPodPattern          = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
	// logLevelPrefix 匹配一行开头的 CRI 前缀（可选）和 slog 默认格式的时间，后面接级别。级别筛选用它排除低于所选级别的行，所以无法识别级别的行（panic、标准库 log 的输出）在 WARN、ERROR 筛选下仍会显示。
	logLevelPrefix = fmt.Sprintf(`^(?:\S{1,%d} (?:stdout|stderr) [FP] )?\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? `, maxLogTimestampBytes)
	logLevelBelow  = map[string]string{"INFO": "DEBUG", "WARN": "DEBUG|INFO", "ERROR": "DEBUG|INFO|WARN"}
)

func (c *AdminLogsConfig) enabled() bool { return c.LokiURL != "" }

func (c *AdminLogsConfig) validate() error {
	// loki_url 为空时整个块只作说明，与 app_id 为 0 的 admin.github 一样不校验其余字段，方便先把选择器写好。
	if c.LokiURL == "" {
		return nil
	}
	c.LokiURL = strings.TrimSuffix(c.LokiURL, "/")
	u, err := url.Parse(c.LokiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.LokiURL, "#") {
		return errors.New("admin logs loki_url must be an http(s) URL without credentials, query or fragment")
	}
	if c.Selector == "" {
		c.Selector = defaultLogSelector
	}
	if len(c.Selector) > 512 || !logSelectorPattern.MatchString(c.Selector) {
		return errors.New(`admin logs selector must be a LogQL stream selector such as {namespace="app",container="msime-backend"}`)
	}
	return nil
}

// logFilter 是两个日志接口共用的筛选条件，都已校验。
type logFilter struct {
	pod, level, q string
}

// logQL 拼出 LogQL。用户输入只以 strconv.Quote 转义后的字符串字面量出现：Loki 的词法分析器用 strconv.Unquote 解析双引号字符串，两者互为逆运算，所以任何输入都只能是一个字符串值，不能改变查询结构。
func (f logFilter) logQL(selector string) string {
	if f.pod == "" && f.level == "" && f.q == "" {
		return selector
	}
	var b strings.Builder
	if f.pod != "" {
		b.WriteString(selector[:len(selector)-1])
		b.WriteString(`,pod=`)
		b.WriteString(strconv.Quote(f.pod))
		b.WriteString("}")
	} else {
		b.WriteString(selector)
	}
	if below := logLevelBelow[f.level]; below != "" {
		b.WriteString(" !~ ")
		b.WriteString(strconv.Quote(logLevelPrefix + "(?:" + below + ") "))
	}
	if f.q != "" {
		b.WriteString(" |= ")
		b.WriteString(strconv.Quote(f.q))
	}
	return b.String()
}

// logsRequest 做两个日志接口共同的检查：方法、功能开关、权限和筛选参数。成功时一并返回已解析的查询参数，供后续校验复用；失败时已写好响应。
func (s *Server) logsRequest(w http.ResponseWriter, r *http.Request) (logFilter, url.Values, bool) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method_not_allowed")
		return logFilter{}, nil, false
	}
	if !s.config.Admin.Logs.enabled() {
		fail(w, 404, "logs_disabled")
		return logFilter{}, nil, false
	}
	if !requirePerm(w, r, account.PermViewLogs) {
		return logFilter{}, nil, false
	}
	query := r.URL.Query()
	f := logFilter{pod: query.Get("pod"), level: strings.ToUpper(query.Get("level")), q: query.Get("q")}
	switch {
	case f.pod != "" && !logPodPattern.MatchString(f.pod):
		fail(w, 400, "invalid_pod")
	case f.level != "" && logLevelBelow[f.level] == "":
		fail(w, 400, "invalid_level")
	case len(f.q) > maxLogQuery || !utf8.ValidString(f.q) || strings.IndexFunc(f.q, unicode.IsControl) >= 0:
		fail(w, 400, "invalid_query")
	default:
		return f, query, true
	}
	return logFilter{}, nil, false
}

// logSince 解析 since（Go 时长写法，例如 15m、1h），范围 1 分钟到 24 小时。
func logSince(query url.Values) (time.Duration, bool) {
	v := query.Get("since")
	if v == "" {
		return defaultLogSince, true
	}
	d, err := time.ParseDuration(v)
	return d, err == nil && d >= time.Minute && d <= maxLogSince
}

// lokiEntry 是 Loki 返回的一行：纳秒时间戳、所属副本和原始内容。
type lokiEntry struct {
	ns   int64
	pod  string
	line string
}

// lokiValues 预留单个日志流的常见上限，避免 encoding/json 为每行结果反复扩容切片。
type lokiValues [][2]string

func (v *lokiValues) UnmarshalJSON(data []byte) error {
	capacity := min(maxLogLimit, len(data)/32)
	if capacity == 0 && len(data) > 2 {
		capacity = 1
	}
	values := make([][2]string, 0, capacity)
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	*v = values
	return nil
}

// logLineJSON 是返回给前端的一行。id 是按时间戳、副本及完整原文生成的稳定去重标识；ts 是 Loki 的纳秒时间戳（十进制字符串，也是续传游标）；time 和 stream 取自 CRI 前缀，没有前缀时 time 用 Loki 的时间戳、stream 为空；level 是从 slog 默认格式里解析出的级别，无法识别时为空；message 去掉了 CRI 前缀和 slog 的时间与级别。
type logLineJSON struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	Time    string `json:"time"`
	Pod     string `json:"pod"`
	Stream  string `json:"stream"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

func (e lokiEntry) JSON() logLineJSON {
	return e.jsonWithHash(e.key().hash)
}

func (e lokiEntry) jsonWithHash(hash uint64) logLineJSON {
	ts := strconv.FormatInt(e.ns, 10)
	out := logLineJSON{ID: ts + ":" + strconv.FormatUint(hash, 16), TS: ts, Pod: e.pod}
	rest := e.line
	if ts, stream, after, ok := cutCRIPrefix(rest); ok {
		out.Time, out.Stream, rest = ts, stream, after
	} else {
		out.Time = time.Unix(0, e.ns).UTC().Format(time.RFC3339Nano)
	}
	if level, after, ok := cutSlogPrefix(rest); ok {
		out.Level, rest = level, after
	}
	out.Message = truncateUTF8(rest, maxLogMessageBytes)
	return out
}

// cutCRIPrefix 拆开 CRI 日志格式「<RFC3339Nano 时间> <stdout|stderr> <F|P> <内容>」。
func cutCRIPrefix(line string) (ts, stream, rest string, ok bool) {
	end := strings.IndexByte(line[:min(len(line), maxLogTimestampBytes+1)], ' ')
	if end < 0 {
		return "", "", "", false
	}
	ts, rest = line[:end], line[end+1:]
	// 流名称与标签的长度固定，不能在异常正文中继续查找分隔符。
	const streamBytes, suffixBytes = len("stderr"), len("stderr F ")
	if len(rest) < suffixBytes {
		return "", "", "", false
	}
	stream = rest[:streamBytes]
	suffix := rest[streamBytes:suffixBytes]
	if (stream != "stdout" && stream != "stderr") || (suffix != " F " && suffix != " P ") {
		return "", "", "", false
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		return "", "", "", false
	}
	return ts, stream, rest[suffixBytes:], true
}

// cutSlogPrefix 拆开 slog 默认处理器（经标准库 log 输出）的「2026/10/01 14:47:01 INFO 内容」，与 logLevelPrefix 的识别规则一致。
func cutSlogPrefix(line string) (level, rest string, ok bool) {
	const layout = "0000/00/00 00:00:00"
	if len(line) < len(layout)+1 {
		return "", "", false
	}
	for i := 0; i < len(layout); i++ {
		if layout[i] == '0' {
			if line[i] < '0' || line[i] > '9' {
				return "", "", false
			}
		} else if line[i] != layout[i] {
			return "", "", false
		}
	}
	rest = line[len(layout):]
	if strings.HasPrefix(rest, ".") {
		digits := strings.IndexFunc(rest[1:], func(c rune) bool { return c < '0' || c > '9' })
		if digits <= 0 {
			return "", "", false
		}
		rest = rest[1+digits:]
	}
	rest, ok = strings.CutPrefix(rest, " ")
	if !ok {
		return "", "", false
	}
	level, rest, ok = strings.Cut(rest, " ")
	if !ok || (level != "DEBUG" && level != "INFO" && level != "WARN" && level != "ERROR") {
		return "", "", false
	}
	return level, rest, true
}

func truncateUTF8(v string, n int) string {
	if len(v) > n {
		for n > 0 && !utf8.RuneStart(v[n]) {
			n--
		}
		v = v[:n]
	}
	return strings.ToValidUTF8(v, "\uFFFD")
}

// errLoki 是 Loki 请求失败的原因。它不含请求地址：地址里有管理员输入的搜索词，不写进日志。
type errLoki struct{ reason string }

func (e errLoki) Error() string { return "loki " + e.reason }

// lokiGet 请求 Loki 并把 JSON 响应解到 v；响应正文缓冲在常见大小内复用，避免每次查询都重新分配整份正文。
func (s *Server) lokiGet(ctx context.Context, path string, params url.Values, v any) error {
	ctx, cancel := context.WithTimeout(ctx, lokiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.Admin.Logs.LokiURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return errLoki{"request invalid"}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.loki.Do(req)
	if err != nil {
		return errLoki{"unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errLoki{fmt.Sprintf("status %d", resp.StatusCode)}
	}
	buffer := lokiResponseBufferPool.Get()
	var body *bytes.Buffer
	if buffer == nil {
		body = new(bytes.Buffer)
	} else {
		body = buffer.(*bytes.Buffer)
		body.Reset()
	}
	if resp.ContentLength >= bytes.MinRead && resp.ContentLength <= maxLokiResponseBytes {
		// 声明长度只作分配提示；复用已有容量，并预留 EOF 探测空间。
		capacity := int(resp.ContentLength) + bytes.MinRead
		if body.Cap() < capacity {
			body = bytes.NewBuffer(make([]byte, 0, capacity))
		}
	}
	defer func() {
		if body.Cap() <= maxLokiPooledResponseBytes {
			body.Reset()
			lokiResponseBufferPool.Put(body)
		}
	}()
	if _, err := body.ReadFrom(io.LimitReader(resp.Body, maxLokiResponseBytes+1)); err != nil {
		return errLoki{"read failed"}
	}
	if body.Len() > maxLokiResponseBytes {
		return errLoki{"response too large"}
	}
	if err := json.Unmarshal(body.Bytes(), v); err != nil {
		return errLoki{"invalid response"}
	}
	return nil
}

// lokiQueryRange 调用 query_range，返回 [start, end) 内按 direction 取前 limit 行。Loki 的 limit 作用于所有流合并后的结果，结果按流分组，这里拍平但不排序。
func (s *Server) lokiQueryRange(ctx context.Context, query string, start, end int64, limit int, direction string) ([]lokiEntry, error) {
	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values lokiValues        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	params := url.Values{"query": {query}, "start": {strconv.FormatInt(start, 10)}, "end": {strconv.FormatInt(end, 10)}, "limit": {strconv.Itoa(limit)}, "direction": {direction}}
	if err := s.lokiGet(ctx, "/loki/api/v1/query_range", params, &body); err != nil {
		return nil, err
	}
	if body.Status != "success" || body.Data.ResultType != "streams" {
		return nil, errLoki{"unexpected result"}
	}
	// Loki 已返回所有流，先统计总行数，避免展开结果时反复扩容。
	count := 0
	for _, stream := range body.Data.Result {
		count += len(stream.Values)
	}
	var entries []lokiEntry
	if count > 0 {
		entries = make([]lokiEntry, 0, count)
	}
	for _, stream := range body.Data.Result {
		for _, value := range stream.Values {
			ns, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				return nil, errLoki{"invalid timestamp"}
			}
			entries = append(entries, lokiEntry{ns: ns, pod: stream.Stream["pod"], line: value[1]})
		}
	}
	return entries, nil
}

// lokiPods 返回截至 at、长度为 window 的窗口内有日志的副本名，不受其他筛选条件影响，供前端显示每个副本的筛选按钮。用 count_over_time 的即时查询而不是标签值接口：后者按索引和块的时间粒度回答，会带上一两个小时前早已下线的副本。
func (s *Server) lokiPods(ctx context.Context, window time.Duration, at time.Time) ([]string, error) {
	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	query := fmt.Sprintf("sum by (pod) (count_over_time(%s [%ds]))", s.config.Admin.Logs.Selector, int64(window/time.Second))
	params := url.Values{"query": {query}, "time": {strconv.FormatInt(at.UnixNano(), 10)}}
	if err := s.lokiGet(ctx, "/loki/api/v1/query", params, &body); err != nil {
		return nil, err
	}
	if body.Status != "success" || body.Data.ResultType != "vector" {
		return nil, errLoki{"unexpected result"}
	}
	pods := make([]string, 0, min(len(body.Data.Result), maxLogPods))
	for _, series := range body.Data.Result {
		if pod := series.Metric["pod"]; logPodPattern.MatchString(pod) && len(pods) < maxLogPods {
			pods = append(pods, pod)
		}
	}
	slices.Sort(pods)
	return pods, nil
}

// sortLogEntries 按时间升序排列，最新的在最后；时间相同的按副本和内容排，保证结果稳定。
func sortLogEntries(entries []lokiEntry) {
	slices.SortStableFunc(entries, compareLogEntries)
}

func compareLogEntries(a, b lokiEntry) int {
	if a.ns != b.ns {
		if a.ns < b.ns {
			return -1
		}
		return 1
	}
	if c := strings.Compare(a.pod, b.pod); c != 0 {
		return c
	}
	return strings.Compare(a.line, b.line)
}

func logLinesJSON[E interface{ JSON() logLineJSON }](entries []E) []logLineJSON {
	out := make([]logLineJSON, len(entries))
	for i, e := range entries {
		out[i] = e.JSON()
	}
	return out
}

func lokiUnavailable(w http.ResponseWriter, err error) {
	slog.Warn("admin logs: loki query failed", "reason", err.Error())
	fail(w, 502, "logs_unavailable")
}

// adminLogs 服务 GET /api/logs?since=&limit=&pod=&level=&q=，需要 view_logs：返回时间窗口内最新的 limit 行（按时间升序，最新的在最后）和窗口内出现过的副本。
func (s *Server) adminLogs(w http.ResponseWriter, r *http.Request) {
	f, query, ok := s.logsRequest(w, r)
	if !ok {
		return
	}
	since, ok := logSince(query)
	if !ok {
		fail(w, 400, "invalid_since")
		return
	}
	limit, ok := intQuery(query, "limit", defaultLogLimit, maxLogLimit)
	if !ok {
		fail(w, 400, "invalid_limit")
		return
	}
	ctx := r.Context()
	end := time.Now()
	start := end.Add(-since)
	entries, err := s.lokiQueryRange(ctx, f.logQL(s.config.Admin.Logs.Selector), start.UnixNano(), end.UnixNano(), limit, "backward")
	if err != nil {
		lokiUnavailable(w, err)
		return
	}
	pods, err := s.lokiPods(ctx, since, end)
	if err != nil {
		lokiUnavailable(w, err)
		return
	}
	sortLogEntries(entries)
	// cursor 是读到的位置（窗口终点），可以交给日志流从这里继续。
	respond(w, 200, map[string]any{
		"lines": logLinesJSON(entries), "pods": pods, "truncated": len(entries) >= limit,
		"cursor": strconv.FormatInt(end.UnixNano(), 10), "since": start.UTC(), "until": end.UTC(),
	})
}

// isAdminHost 判断请求是否发往管理后台域名。
func (s *Server) isAdminHost(r *http.Request) bool {
	if !s.config.Admin.Enabled {
		return false
	}
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return strings.EqualFold(host, s.config.Admin.Host)
}

// isAdminLogStream 判断请求是否是日志流本身：它是长连接，既不写请求日志，也不受后台 15 秒请求超时限制。
func (s *Server) isAdminLogStream(r *http.Request) bool {
	return r.URL.Path == adminLogsStreamPath && s.isAdminHost(r)
}

// logStreamLimiter 限制同时打开的日志流：每个管理员 logStreamsPerActor 个，每个副本合计 logStreamsTotal 个。计数在进程内存里，按副本计算，见 README「多副本部署」。
type logStreamLimiter struct {
	mu      sync.Mutex
	total   int
	byActor map[string]int
}

func (l *logStreamLimiter) acquire(actor string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= logStreamsTotal || l.byActor[actor] >= logStreamsPerActor {
		return nil, false
	}
	if l.byActor == nil {
		l.byActor = map[string]int{}
	}
	l.total++
	l.byActor[actor]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.byActor[actor]--; l.byActor[actor] <= 0 {
				delete(l.byActor, actor)
			}
		})
	}, true
}

// EndLogStreams 结束所有日志流，供 http.Server.RegisterOnShutdown 调用：Shutdown 会等待进行中的请求，长连接不主动结束会拖满整个优雅关闭时限。
func (s *Server) EndLogStreams() { s.logStreamsOnce.Do(func() { close(s.logStreamsDone) }) }

// logKey 是去重用的键：纳秒时间戳加上副本名与内容的 FNV 摘要。
type logKey struct {
	ns   int64
	hash uint64
}

func (e lokiEntry) key() logKey {
	const (
		fnvOffset = uint64(14695981039346656037)
		fnvPrime  = uint64(1099511628211)
	)
	hash := fnvOffset
	for i := 0; i < len(e.pod); i++ {
		hash ^= uint64(e.pod[i])
		hash *= fnvPrime
	}
	// 分隔符仍需参与一次乘法，保持 pod 和 line 的边界语义。
	hash *= fnvPrime
	for i := 0; i < len(e.line); i++ {
		hash ^= uint64(e.line[i])
		hash *= fnvPrime
	}
	return logKey{e.ns, hash}
}

// logStreamEntry 在流的去重与输出阶段携带已计算的摘要；原始 Loki 行保持只读。
type logStreamEntry struct {
	lokiEntry
	hash uint64
}

func (e logStreamEntry) JSON() logLineJSON {
	return e.lokiEntry.jsonWithHash(e.hash)
}

// logTail 是一个日志流的读取进度。
type logTail struct {
	query string
	// cursor 不越过尚未读完的时间戳，供客户端续传；floor 及更早的行不再发送。next 是分页继续读取的位置。
	cursor, floor int64
	next          int64
	seen          map[logKey]struct{}
	// readAt 记录分页边界时间戳已读取的原始行数，包含重复行；负数表示超限且已经提示缺口。
	readAt map[int64]int
	// busy 表示上一次轮询用满了页数，这次不回看，先追上进度。
	busy bool
}

type logGap struct{ from, to int64 }

// pollLogs 读取到 now 为止的新行，回看并去重。同时间戳分页按已读行数扩大请求，发送额度仍为每页五百条；超过单时间戳上限或落后太多时返回明确的缺口范围。
func (s *Server) pollLogs(ctx context.Context, t *logTail, now int64) (out []logStreamEntry, gaps []logGap, err error) {
	if t.readAt == nil {
		t.readAt = map[int64]int{}
	}
	if t.busy && now-t.cursor > int64(logStreamMaxLag) {
		gaps = append(gaps, logGap{t.cursor, now - int64(logStreamOverlap)})
		t.cursor = now - int64(logStreamOverlap)
		t.floor = t.cursor - 1
		t.busy = false
		t.next = 0
		clear(t.seen)
		clear(t.readAt)
	}
	start := t.cursor
	if t.busy && t.next > 0 {
		start = t.next
	} else if !t.busy {
		start -= int64(logStreamOverlap)
	}
	start = max(start, t.floor+1)
	for page := 0; page < logStreamPages && start < now; page++ {
		if t.readAt[start] < 0 {
			start++
			if start >= now {
				t.busy = false
				t.next = 0
				break
			}
		}
		limit := min(logStreamPageSize+max(t.readAt[start], 0), logStreamMaxBatchSize) + 1
		entries, err := s.lokiQueryRange(ctx, t.query, start, now, limit, "forward")
		if err != nil {
			return out, gaps, err
		}
		// Loki 按流分组返回，必须先合并排序，才能按总时间顺序保留前瞻行。
		sortLogEntries(entries)
		n, added := 0, 0
		for n < len(entries) && n < limit-1 {
			e := entries[n]
			if e.ns <= t.floor || t.readAt[e.ns] < 0 {
				n++
				continue
			}
			k := e.key()
			if _, dup := t.seen[k]; dup {
				n++
				continue
			}
			if added == logStreamPageSize {
				break
			}
			t.seen[k] = struct{}{}
			out = append(out, logStreamEntry{lokiEntry: e, hash: k.hash})
			added++
			n++
		}
		if n == len(entries) {
			t.busy = false
			t.next = 0
			break
		}
		last := entries[n-1].ns
		count := 0
		for i := n - 1; i >= 0 && entries[i].ns == last; i-- {
			count++
		}
		if t.readAt[last] >= 0 {
			t.readAt[last] = max(t.readAt[last], count)
		}
		start = last + 1
		if entries[n].ns == last && t.readAt[last] >= 0 {
			if t.readAt[last] >= logStreamMaxBatchSize {
				gaps = append(gaps, logGap{last, last + 1})
				t.readAt[last] = -1
			} else {
				start = last
			}
		}
		t.next = start
		t.cursor = start - 1
		t.busy = true
	}
	if !t.busy {
		t.cursor = max(t.cursor, now)
	}
	// 早于下一次回看起点的键不会再出现，清掉以免集合无限增长。
	horizon := t.cursor - int64(logStreamOverlap)
	for k := range t.seen {
		if k.ns < horizon {
			delete(t.seen, k)
		}
	}
	for ns := range t.readAt {
		if ns < horizon {
			delete(t.readAt, ns)
		}
	}
	slices.SortStableFunc(out, func(a, b logStreamEntry) int { return compareLogEntries(a.lokiEntry, b.lokiEntry) })
	return out, gaps, nil
}

// logStreamAccess 重新读取建立日志流时使用的凭据和当前角色，返回空字符串表示仍可读日志，其余值是结束流的公开原因。
func (s *Server) logStreamAccess(ctx context.Context, r *http.Request) string {
	ctx, cancel := context.WithTimeout(ctx, logStreamAuthTimeout)
	defer cancel()
	actor, email, err := s.adminCredentialIdentity(r.WithContext(ctx))
	if err == nil {
		var access account.AdminAccess
		access, err = s.adminAccess(ctx, actor, email)
		if err == nil {
			if slices.Contains(access.Permissions, account.PermViewLogs) {
				return ""
			}
			return "permission_denied"
		}
	}
	if errors.Is(err, account.ErrInvalid) {
		return "unauthorized"
	}
	return "admin_auth_unavailable"
}

// adminLogsStream 服务 GET /api/logs/stream?pod=&level=&q=&since=&backfill=&cursor=，需要 view_logs。返回 Server-Sent Events：
//
//   - lines：{"lines":[...]}，事件 id 是续传游标；
//   - pods：{"pods":[...]}，since 窗口内有日志的副本，连接时和之后每 30 秒发送一次；
//   - gap：{"from","to"}，读取落后太多或单时间戳行数超限，一段日志被跳过；
//   - error：{"code":"logs_unavailable"}，Loki 暂时不可用，连接保持并继续重试，连续失败 5 次后关闭；
//   - end：{"reason":"..."}，连接满 30 分钟或凭据、权限校验失败后关闭。
//
// 没有新行时每 15 秒发送一行注释作为心跳。不带 cursor（也没有 Last-Event-ID 请求头）时先回填 since 窗口内最新的 backfill 行；带 cursor 时从它之后继续，最早回到一小时前。
func (s *Server) adminLogsStream(w http.ResponseWriter, r *http.Request) {
	f, query, ok := s.logsRequest(w, r)
	if !ok {
		return
	}
	since, ok := logSince(query)
	if !ok {
		fail(w, 400, "invalid_since")
		return
	}
	backfill, ok := httpquery.Int(query, "backfill", defaultLogBackfill, 0, maxLogBackfill)
	if !ok {
		fail(w, 400, "invalid_backfill")
		return
	}
	now := time.Now()
	cursor := int64(0)
	if v := query.Get("cursor"); v != "" || r.Header.Get("Last-Event-ID") != "" {
		if v == "" {
			v = r.Header.Get("Last-Event-ID")
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > now.Add(time.Minute).UnixNano() {
			fail(w, 400, "invalid_cursor")
			return
		}
		cursor = max(n, now.Add(-logStreamMaxCursorAge).UnixNano())
	}
	access, _ := account.AdminAccessFrom(r.Context())
	release, ok := s.logStreams.acquire(access.Actor)
	if !ok {
		w.Header().Set("Retry-After", "30")
		fail(w, 429, "too_many_streams")
		return
	}
	defer release()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.lifetime, cancel)
	defer stop()
	go func() {
		select {
		case <-s.logStreamsDone:
			cancel()
		case <-ctx.Done():
		}
	}()

	rc := http.NewResponseController(w)
	// 服务端的 ReadTimeout 到期会取消请求上下文，长连接要去掉读超时；客户端断开仍能通过连接关闭感知。
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	lastWrite := time.Now()
	write := func(frame []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(logStreamWriteTimeout))
		if _, err := w.Write(frame); err != nil {
			return false
		}
		if err := rc.Flush(); err != nil {
			return false
		}
		lastWrite = time.Now()
		return true
	}
	send := func(event, id string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		return write(logStreamFrame(event, id, data))
	}
	if !write([]byte("retry: 5000\n\n")) {
		return
	}

	t := &logTail{query: f.logQL(s.config.Admin.Logs.Selector), seen: map[logKey]struct{}{}}
	failures := 0
	lokiFailed := func(err error) bool {
		failures++
		slog.Warn("admin logs stream: loki query failed", "reason", err.Error(), "failures", failures)
		if failures == 1 && !send("error", "", map[string]string{"code": "logs_unavailable"}) {
			return false
		}
		return failures < logStreamMaxFailures
	}
	sendPods := func(at time.Time) bool {
		pods, err := s.lokiPods(ctx, since, at)
		if err != nil {
			return ctx.Err() == nil && lokiFailed(err)
		}
		return send("pods", "", map[string]any{"pods": pods})
	}
	backfillPending := cursor == 0 && backfill > 0
	backfillStart := now.Add(-since).UnixNano()
	if cursor > 0 {
		t.cursor, t.floor = cursor, cursor
	} else {
		t.cursor, t.floor = now.UnixNano(), backfillStart-1
		if backfill == 0 {
			// 不回填时，轮询回看也不能补发连接建立之前的日志。
			t.floor = t.cursor - 1
		}
	}
	// 初次回填失败时保留原窗口重试，不能先推进实时游标而永久跳过历史日志。
	sendBackfill := func() bool {
		if !backfillPending {
			return true
		}
		entries, err := s.lokiQueryRange(ctx, t.query, backfillStart, now.UnixNano(), backfill, "backward")
		if err != nil {
			return ctx.Err() == nil && lokiFailed(err)
		}
		sortLogEntries(entries)
		lines := make([]logLineJSON, len(entries))
		for i, e := range entries {
			key := e.key()
			t.seen[key] = struct{}{}
			lines[i] = e.jsonWithHash(key.hash)
		}
		if len(entries) > 0 {
			// 回填只取了最新的 backfill 行，更早的行不再补发，否则会乱序出现在已显示的行之前。
			t.floor = entries[0].ns - 1
		}
		backfillPending = false
		failures = 0
		return send("lines", strconv.FormatInt(t.cursor, 10), map[string]any{"lines": lines})
	}
	if !sendBackfill() {
		return
	}
	if !sendPods(now) {
		return
	}
	podsAt := now
	deadline := now.Add(s.logMaxDuration)
	ticker := time.NewTicker(s.logPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			if tick.After(deadline) {
				send("end", "", map[string]string{"reason": "max_duration"})
				return
			}
			// 长连接必须重新校验；退出、撤销令牌、停用成员或收回权限后不能继续按旧快照查询日志。
			if reason := s.logStreamAccess(ctx, r); reason != "" {
				send("end", "", map[string]string{"reason": reason})
				return
			}
			if backfillPending {
				if !sendBackfill() {
					return
				}
				if time.Since(lastWrite) >= s.logHeartbeat && !write([]byte(": ping\n\n")) {
					return
				}
				continue
			}
			entries, gaps, err := s.pollLogs(ctx, t, tick.UnixNano())
			if ctx.Err() != nil {
				return
			}
			for _, gap := range gaps {
				if !send("gap", "", map[string]string{"from": strconv.FormatInt(gap.from, 10), "to": strconv.FormatInt(gap.to, 10)}) {
					return
				}
			}
			if err != nil {
				if !lokiFailed(err) {
					return
				}
			} else {
				failures = 0
			}
			if len(entries) > 0 && !send("lines", strconv.FormatInt(t.cursor, 10), map[string]any{"lines": logLinesJSON(entries)}) {
				return
			}
			if tick.Sub(podsAt) >= logStreamPodsEvery {
				podsAt = tick
				if !sendPods(tick) {
					return
				}
			}
			if time.Since(lastWrite) >= s.logHeartbeat && !write([]byte(": ping\n\n")) {
				return
			}
		}
	}
}

// logStreamFrame 接管 `json.Marshal` 返回的独占正文缓冲，允许覆写原切片。
// 容量充足时原地移出帧头空间；不足时只为完整 SSE 帧分配一次，保持单次写出。
func logStreamFrame(event, id string, data []byte) []byte {
	prefix := len("event: ") + len(event) + len("\ndata: ")
	if id != "" {
		prefix += len("id: ") + len(id) + 1
	}
	size := prefix + len(data) + 2
	var frame []byte
	if cap(data) >= size {
		frame = data[:size]
	} else {
		frame = make([]byte, size)
	}
	copy(frame[prefix:], data)
	offset := 0
	if id != "" {
		offset += copy(frame[offset:], "id: ")
		offset += copy(frame[offset:], id)
		frame[offset] = '\n'
		offset++
	}
	offset += copy(frame[offset:], "event: ")
	offset += copy(frame[offset:], event)
	copy(frame[offset:], "\ndata: ")
	frame[len(frame)-2], frame[len(frame)-1] = '\n', '\n'
	return frame
}
