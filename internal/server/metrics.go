package server

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// Upstream call metrics (unit U10).

// latencyBoundsMS are the upper bounds of the latency histogram buckets; one more bucket counts everything slower.
var latencyBoundsMS = [...]int{25, 50, 100, 200, 300, 500, 750, 1000, 1500, 2000, 3000, 5000, 8000, 13000, 20000, 30000, 60000, 120000}

const (
	// overflowBucket is the histogram key of calls slower than the last bound.
	overflowBucket = "inf"
	// pendingRetention bounds how long unflushed hourly buckets are kept while the database is unreachable.
	pendingRetention = 7 * 24 * time.Hour
	// pendingMinuteRetention bounds how long unflushed minute buckets are kept while the database is unreachable: older minutes are outside every window the status probe will still judge.
	pendingMinuteRetention = time.Hour
)

type latencyHistogram [len(latencyBoundsMS) + 1]int64

// metricCounts is an aggregate of upstream calls: counts, a latency histogram and the metered usage of the successful calls. It never holds request or response content.
type metricCounts struct {
	calls   int64
	errors  int64
	latency latencyHistogram
	usage   float64
}

func (c *metricCounts) add(o metricCounts) {
	c.calls += o.calls
	c.errors += o.errors
	c.usage += o.usage
	for i, n := range o.latency {
		c.latency[i] += n
	}
}

func (c *metricCounts) observe(latency time.Duration, failed bool, usage float64) {
	c.calls++
	if failed {
		c.errors++
	} else {
		c.usage += usage
	}
	ms := latency.Milliseconds()
	i := 0
	for i < len(latencyBoundsMS) && ms > int64(latencyBoundsMS[i]) {
		i++
	}
	c.latency[i]++
}

// percentile estimates the q-quantile latency in milliseconds by linear interpolation inside the histogram bucket that holds it; ok is false without calls.
func (h latencyHistogram) percentile(q float64) (ms int, ok bool) {
	var total int64
	for _, n := range h {
		total += n
	}
	if total == 0 {
		return 0, false
	}
	rank := int64(math.Ceil(q * float64(total)))
	var seen int64
	for i, n := range h {
		if n == 0 || seen+n < rank {
			seen += n
			continue
		}
		if i == len(latencyBoundsMS) {
			return latencyBoundsMS[len(latencyBoundsMS)-1], true
		}
		lower := 0
		if i > 0 {
			lower = latencyBoundsMS[i-1]
		}
		upper := latencyBoundsMS[i]
		return lower + int(math.Round(float64(upper-lower)*float64(rank-seen)/float64(n))), true
	}
	return latencyBoundsMS[len(latencyBoundsMS)-1], true
}

// histogramJSON is the admin_service_metrics.latency_buckets form: bucket upper bound in milliseconds to count, empty buckets omitted.
func (h latencyHistogram) histogramJSON() map[string]int64 {
	out := map[string]int64{}
	for i, n := range h {
		if n == 0 {
			continue
		}
		key := overflowBucket
		if i < len(latencyBoundsMS) {
			key = strconv.Itoa(latencyBoundsMS[i])
		}
		out[key] = n
	}
	return out
}

// histogramFromJSON reverses histogramJSON; keys that are not bucket bounds land in the nearest bucket above them.
func histogramFromJSON(buckets map[string]int64) latencyHistogram {
	var h latencyHistogram
	for key, n := range buckets {
		if n <= 0 {
			continue
		}
		i := len(latencyBoundsMS)
		if bound, err := strconv.Atoi(key); err == nil {
			i = 0
			for i < len(latencyBoundsMS) && bound > latencyBoundsMS[i] {
				i++
			}
		}
		h[i] += n
	}
	return h
}

type hourKey struct {
	service string
	hour    int64
}

// minuteKey is a service and the Unix time of the start of a UTC minute.
type minuteKey struct {
	service string
	minute  int64
}

// serviceMetrics 保存本副本的小时、分钟增量和待确认批次；小时用于云端监控，分钟供主副本汇总判定状态。零值可直接使用。
type serviceMetrics struct {
	mu      sync.Mutex
	hours   map[hourKey]*metricCounts
	minutes map[minuteKey]*metricCounts
	flushMu sync.Mutex
	batch   *account.ServiceMetricBatch
}

// record adds one call that finished at at.
func (m *serviceMetrics) record(service string, at time.Time, latency time.Duration, failed bool, usage float64) {
	at = at.UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hours == nil {
		m.hours = map[hourKey]*metricCounts{}
	}
	if m.minutes == nil {
		m.minutes = map[minuteKey]*metricCounts{}
	}
	key := hourKey{service, at.Truncate(time.Hour).Unix()}
	bucket := m.hours[key]
	if bucket == nil {
		bucket = &metricCounts{}
		m.hours[key] = bucket
	}
	bucket.observe(latency, failed, usage)
	mkey := minuteKey{service, at.Truncate(time.Minute).Unix()}
	slot := m.minutes[mkey]
	if slot == nil {
		slot = &metricCounts{}
		m.minutes[mkey] = slot
	}
	slot.observe(latency, failed, usage)
}

func minuteRows(minutes map[minuteKey]*metricCounts) []account.ServiceMinute {
	rows := make([]account.ServiceMinute, 0, len(minutes))
	for key, c := range minutes {
		rows = append(rows, account.ServiceMinute{Service: key.service, Minute: time.Unix(key.minute, 0).UTC(), Calls: c.calls, Errors: c.errors, Latency: c.latency.histogramJSON()})
	}
	slices.SortFunc(rows, func(a, b account.ServiceMinute) int {
		return cmp.Or(a.Minute.Compare(b.Minute), cmp.Compare(a.Service, b.Service))
	})
	return rows
}

// pending 返回待写入和待确认的小时增量快照，不移除原数据。
func (m *serviceMetrics) pending() []account.ServiceMetric {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := metricRows(m.hours)
	if m.batch != nil {
		rows = append(rows, m.batch.Hours...)
	}
	return rows
}

// metricRows lists hourly buckets in (hour, service) order: every replica upserts the same rows each minute, and a common order keeps their transactions from deadlocking.
func metricRows(hours map[hourKey]*metricCounts) []account.ServiceMetric {
	rows := make([]account.ServiceMetric, 0, len(hours))
	for key, c := range hours {
		rows = append(rows, account.ServiceMetric{Service: key.service, Hour: time.Unix(key.hour, 0).UTC(), Calls: c.calls, Errors: c.errors, Latency: c.latency.histogramJSON(), Usage: c.usage})
	}
	slices.SortFunc(rows, func(a, b account.ServiceMetric) int {
		return cmp.Or(a.Hour.Compare(b.Hour), cmp.Compare(a.Service, b.Service))
	})
	return rows
}

func rowCounts(row account.ServiceMetric) metricCounts {
	return metricCounts{calls: row.Calls, errors: row.Errors, latency: histogramFromJSON(row.Latency), usage: row.Usage}
}

// flushMetrics 按批次写回小时与分钟增量，失败时保留原批次标识，供每分钟或退出时安全重试。
func (s *Server) flushMetrics(ctx context.Context) error {
	return s.metrics.flush(ctx, s.accounts.RecordServiceMetricBatch)
}

// flush 串行处理待确认批次；新调用留在独立桶内，旧批次确认后在同一轮继续写回。
func (m *serviceMetrics) flush(ctx context.Context, write func(context.Context, account.ServiceMetricBatch) error) error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	for range 2 {
		batch := m.nextBatch(time.Now())
		if batch == nil {
			return nil
		}
		if err := write(ctx, *batch); err != nil {
			return err
		}
		m.mu.Lock()
		m.batch = nil
		m.mu.Unlock()
	}
	return nil
}

// nextBatch 在数据库故障期间也裁剪过期桶；重试只裁剪过期行，绝不把新调用并入旧标识。
func (m *serviceMetrics) nextBatch(now time.Time) *account.ServiceMetricBatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.hours {
		if now.Sub(time.Unix(key.hour, 0)) > pendingRetention {
			delete(m.hours, key)
		}
	}
	for key := range m.minutes {
		if now.Sub(time.Unix(key.minute, 0)) > pendingMinuteRetention {
			delete(m.minutes, key)
		}
	}
	if m.batch != nil {
		m.batch.Hours = slices.DeleteFunc(m.batch.Hours, func(row account.ServiceMetric) bool { return now.Sub(row.Hour) > pendingRetention })
		m.batch.Minutes = slices.DeleteFunc(m.batch.Minutes, func(row account.ServiceMinute) bool { return now.Sub(row.Minute) > pendingMinuteRetention })
		if len(m.batch.Hours)+len(m.batch.Minutes) == 0 {
			m.batch = nil
		}
	}
	if m.batch == nil && len(m.hours)+len(m.minutes) > 0 {
		m.batch = &account.ServiceMetricBatch{ID: adminRandom(), Hours: metricRows(m.hours), Minutes: minuteRows(m.minutes)}
		m.hours, m.minutes = nil, nil
	}
	return m.batch
}

// meterKey tags a request context with the metered call its upstream request is recorded as.
type meterKey struct{}

// meterCall is one metered upstream call. doUpstream and niuTransRequestWithType fill in the exchange; the handler that tagged the request then settles it once it has validated the response, so an upstream that answers 2xx with an error body (Tencent's Response.Error, NiuTrans' errorCode, a non-SUCCESS cloud reply) still counts as a failure. A handler's request goroutine owns its meterCall, so it needs no lock.
type meterCall struct {
	service string
	// usage is the metered quantity a successful call consumes: characters or seconds, or 0 for services metered by calls.
	usage   float64
	sent    bool
	latency time.Duration
	err     error
}

// metered tags r so that the upstream call made for it through doUpstream or niuTransRequestWithType is captured in the returned meterCall, which the caller passes to settleMeter after validating the response.
func metered(r *http.Request, service string, usage float64) (*http.Request, *meterCall) {
	call := &meterCall{service: service, usage: usage}
	return r.WithContext(context.WithValue(r.Context(), meterKey{}, call)), call
}

// meterUsage replaces the usage of the call r is tagged with, for a handler that learns its metered quantity only later (Tencent meters only its cache misses); untagged requests are ignored.
func meterUsage(r *http.Request, usage float64) {
	if call, ok := r.Context().Value(meterKey{}).(*meterCall); ok {
		call.usage = usage
	}
}

// captureMeter stores the outcome of the upstream exchange that started at started in the call ctx is tagged with; untagged contexts are ignored.
func captureMeter(ctx context.Context, started time.Time, err error) {
	if call, ok := ctx.Value(meterKey{}).(*meterCall); ok {
		call.sent, call.latency, call.err = true, time.Since(started), err
	}
}

// settleMeter records call once its handler has judged the response: it failed when the exchange failed or the response was not accepted. Nothing is recorded when no upstream request was sent (a fully cached translation, a request rejected before the call).
func (s *Server) settleMeter(call *meterCall, accepted bool) {
	if !call.sent || errors.Is(call.err, context.Canceled) {
		return
	}
	err := call.err
	if err == nil && !accepted {
		err = errUpstreamRejected
	}
	s.recordCall(call.service, time.Now(), call.latency, err, call.usage)
}

// errUpstreamRejected marks a call whose 2xx response the handler rejected.
var errUpstreamRejected = errors.New("upstream response rejected")

// observe records one upstream call that started at started and ended now with err. A call the client abandoned is not recorded, because its latency and outcome say nothing about the upstream.
func (s *Server) observe(service string, started time.Time, err error, usage float64) {
	now := time.Now()
	s.recordCall(service, now, now.Sub(started), err, usage)
}

// recordCall records one finished upstream call unless the client abandoned it.
func (s *Server) recordCall(service string, at time.Time, latency time.Duration, err error, usage float64) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.observeCall(service, at, latency, err != nil, usage)
}

// observeCall records one upstream call with an explicit latency, for calls whose latency is not their duration (a streaming session's handshake).
func (s *Server) observeCall(service string, at time.Time, latency time.Duration, failed bool, usage float64) {
	if !s.config.Admin.Enabled {
		return
	}
	s.metrics.record(service, at, latency, failed, usage)
}

// textChars is the number of characters a translation call meters.
func textChars(texts []string) float64 {
	n := 0
	for _, text := range texts {
		n += utf8.RuneCountInString(text)
	}
	return float64(n)
}

// wavSeconds is the duration of a WAV file already accepted by validWAV, from its fmt byte rate and data chunk size.
func wavSeconds(audio []byte) float64 {
	var byteRate, dataBytes uint32
	for offset := 12; offset+8 <= len(audio); {
		name := string(audio[offset : offset+4])
		size := binary.LittleEndian.Uint32(audio[offset+4 : offset+8])
		offset += 8
		if uint64(size) > uint64(len(audio)-offset) {
			break
		}
		switch name {
		case "fmt ":
			if size >= 12 {
				byteRate = binary.LittleEndian.Uint32(audio[offset+8 : offset+12])
			}
		case "data":
			dataBytes = size
		}
		offset += int(size) + int(size&1)
	}
	if byteRate == 0 {
		return 0
	}
	return float64(dataBytes) / float64(byteRate)
}

// monitoredService is one upstream service shown on the cloud and status pages.
type monitoredService struct {
	Key, Name, Provider string
	SlowMS              int
	QuotaLimit          float64
	QuotaUnit           string
	UnitPrice           float64
}

// Meter units: how each recorded service measures usage.
const (
	meterCalls   = "calls"
	meterChars   = "chars"
	meterSeconds = "seconds"
)

// serviceMeter is the unit a service's recorded usage is in; services without a natural quantity are metered by calls.
func serviceMeter(key string) string {
	switch key {
	case "translation":
		return meterChars
	case "transcription", "streaming":
		return meterSeconds
	}
	return meterCalls
}

var defaultServiceNames = map[string]string{
	"cloud": "云候选", "chat": "AI 联想", "translation": "在线翻译", "transcription": "语音识别", "streaming": "实时语音",
	"images": "皮肤生成", "niutrans_document": "文档翻译", "niutrans_image": "图片翻译", "niutrans_voice": "语音翻译",
}

// derivedSlowMS is the slow threshold of a derived service whose calls normally take longer than admin.services' 3000ms default: generating an image or transcribing a whole recording or file routinely takes many seconds, and judging them by 3s would keep them degraded and open incidents for normal traffic.
var derivedSlowMS = map[string]int{"images": 60000, "transcription": 10000, "niutrans_document": 10000, "niutrans_image": 10000, "niutrans_voice": 10000}

var translationProviders = map[string]string{"tencent": "腾讯 TMT", "deepl": "DeepL", "niutrans": "小牛翻译", "openai": "OpenAI 兼容接口"}

// monitoredServices is admin.services when configured, otherwise every upstream this deployment has configured, under its default name, with the upstream host as provider. An admin.services entry keyed "database" is left out: that key holds the status probe's own database pings, rollups and incidents.
func (s *Server) monitoredServices() []monitoredService {
	if len(s.config.Admin.Services) > 0 {
		services := make([]monitoredService, 0, len(s.config.Admin.Services))
		for _, v := range s.config.Admin.Services {
			if v.Key == databaseService {
				continue
			}
			slow := v.SlowMS
			if slow <= 0 {
				slow = defaultServiceSlowMS
			}
			services = append(services, monitoredService{Key: v.Key, Name: v.Name, Provider: v.Provider, SlowMS: slow, QuotaLimit: v.Quota.Limit, QuotaUnit: v.Quota.Unit, UnitPrice: v.Quota.UnitPrice})
		}
		return services
	}
	// Read through a pointer instead of copying Config: this runs on the status probe goroutine, which must touch only the upstream fields it reports.
	c := &s.config
	var services []monitoredService
	add := func(key, provider string) {
		slow := derivedSlowMS[key]
		if slow == 0 {
			slow = defaultServiceSlowMS
		}
		services = append(services, monitoredService{Key: key, Name: defaultServiceNames[key], Provider: provider, SlowMS: slow})
	}
	if c.Cloud.URL != "" {
		add("cloud", urlHost(c.Cloud.URL))
	}
	if c.Chat.URL != "" {
		add("chat", urlHost(c.Chat.URL))
	}
	for _, e := range append([]TranslationEndpoint{c.Translation}, c.TranslationFallbacks...) {
		if e.URL != "" {
			provider := translationProviders[e.Provider]
			if provider == "" {
				provider = urlHost(e.URL)
			}
			add("translation", provider)
			break
		}
	}
	if c.Transcription.URL != "" {
		add("transcription", urlHost(c.Transcription.URL))
	}
	if c.Streaming.URL != "" {
		provider := c.Streaming.Provider
		if provider == "" {
			provider = urlHost(c.Streaming.URL)
		}
		add("streaming", provider)
	}
	if c.Images.URL != "" {
		add("images", urlHost(c.Images.URL))
	}
	for _, v := range []struct {
		key string
		e   NiuTransEndpoint
	}{{"niutrans_document", c.NiuTrans.Document}, {"niutrans_image", c.NiuTrans.Image}, {"niutrans_voice", c.NiuTrans.Voice}} {
		if niuTransEndpointReady(v.e) {
			add(v.key, translationProviders["niutrans"])
		}
	}
	return services
}

// urlHost is the host of an upstream URL, shown as the provider of an unnamed service; it never includes credentials, path or query.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
