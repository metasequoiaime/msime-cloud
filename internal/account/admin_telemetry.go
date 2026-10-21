package account

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// TelemetryPath 是服务端经 Route 挂载的匿名事件接口；IsPath 让它绕过 Bearer 中间件，accountRouteRate 按这个路由给它独立的额度，所以服务端必须原样挂载在这个路径上。
const TelemetryPath = "/v1/telemetry/events"

const (
	// telemetryRateLimit 是遥测事件按地址每分钟的上限，单独一份额度，上报不会挤占登录和社区接口的每分钟 120 次。客户端每次启动只发几条事件，60 次足够补发离线队列。
	telemetryRateLimit = 60
	// telemetryCrashDailyLimit 限制一个地址每天能记录的 crash 事件数，单个来源无法刷满崩溃分组或触发激增通知。
	telemetryCrashDailyLimit = 20
	// telemetryMessageLimit 和 telemetryStackLimit 是崩溃文本的上限（按 Unicode 标量计）；更长的堆栈在行边界处截断，而不是被拒绝。
	telemetryMessageLimit = 1000
	telemetryStackLimit   = 16000
)

// telemetryKinds are the accepted event kinds: download and crash from the first protocol, and the anonymous device activity kinds that feed active devices and crash-free session rates.
// telemetryActivityRetentionDays is how long Prune keeps active, session and session_crash events; the overview reads at most the last 60 days of them.
const telemetryActivityRetentionDays = 90

var telemetryKinds = map[string]bool{"download": true, "crash": true, "active": true, "session": true, "session_crash": true}

var (
	// telemetryChannel is a distribution channel key such as github or cn-mirror; the console maps known keys to labels.
	telemetryChannel = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// telemetryInstallID is an anonymous random installation id; it must not carry user or hardware identifiers.
	telemetryInstallID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
)

// telemetryEvent 是 `POST /v1/telemetry/events` 的请求体。第一版协议之后新增的字段都是可选的，旧客户端无需修改；未知字段返回 400 `invalid_json`。
type telemetryEvent struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
	Message   string `json:"message"`
	Stack     string `json:"stack"`
	Artifact  string `json:"artifact"`
	Channel   string `json:"channel"`
	InstallID string `json:"install_id"`
}

// valid applies the boundary rules: only crashes carry a message and stack (and must have a message), an active event must name its anonymous installation, and the optional download dimensions are short single-line values.
func (v telemetryEvent) valid() bool {
	if !resourceText(v.ID, 16, 128, false) || !telemetryKinds[v.Kind] || !resourceText(v.Platform, 1, 32, false) || !resourceText(v.Version, 1, 64, false) || !resourceText(v.Message, 0, telemetryMessageLimit, true) || !resourceText(v.Stack, 0, telemetryStackLimit, true) {
		return false
	}
	if v.Kind == "crash" {
		if strings.TrimSpace(v.Message) == "" {
			return false
		}
	} else if v.Message != "" || v.Stack != "" {
		return false
	}
	if v.Artifact != "" && !resourceText(v.Artifact, 1, 64, false) {
		return false
	}
	if v.Channel != "" && !telemetryChannel.MatchString(v.Channel) {
		return false
	}
	if v.InstallID != "" && !telemetryInstallID.MatchString(v.InstallID) {
		return false
	}
	return v.Kind != "active" || v.InstallID != ""
}

// telemetryNull stores an omitted optional field as NULL, which the column checks require instead of an empty string.
func telemetryNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// telemetryLines 把 CRLF 和单独的 CR 换行统一为 LF，让 Windows 的堆栈或消息通过控制字符检查，并与以 LF 上报的同一崩溃归为一组。
func telemetryLines(s string) string {
	first := strings.IndexByte(s, '\r')
	if first < 0 {
		return s
	}
	pairs := strings.Count(s[first:], "\r\n")
	if pairs == 0 {
		return strings.ReplaceAll(s, "\r", "\n")
	}
	carriages := strings.Count(s[first:], "\r")
	var out strings.Builder
	// 每对 CRLF 缩短一个字节，按最终长度分配，避免混合换行复制两次或密集 CRLF 保留多余空间。
	out.Grow(len(s) - pairs)
	start := 0
	if carriages == pairs {
		// 纯 CRLF 每次固定跳过两个字节，避免为密集换行逐个判断后继字符。
		for range pairs {
			end := start + strings.IndexByte(s[start:], '\r')
			out.WriteString(s[start:end])
			out.WriteByte('\n')
			start = end + 2
		}
	} else {
		for range carriages {
			end := start + strings.IndexByte(s[start:], '\r')
			out.WriteString(s[start:end])
			out.WriteByte('\n')
			start = end + 1
			if start < len(s) && s[start] == '\n' {
				start++
			}
		}
	}
	out.WriteString(s[start:])
	return out.String()
}

// truncateStack 把超过 limit 个标量的堆栈截到能放下的最后一个完整行，超长堆栈仍被记录而不是被拒绝（否则用同一 ID 重试永远不会成功）。第一行就超过 limit 时在 limit 处截断。
func truncateStack(stack string, limit int) string {
	if utf8.RuneCountInString(stack) <= limit {
		return stack
	}
	cut, n := len(stack), 0
	for i := range stack {
		if n == limit {
			cut = i
			break
		}
		n++
	}
	head := stack[:cut]
	// 截断点正好落在换行符上时 head 已经以完整的行结尾。
	if stack[cut] == '\n' {
		return head
	}
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		return head[:i]
	}
	return head
}

// Telemetry 处理 `POST /v1/telemetry/events`，不需要认证：它经 `Route` 挂载，计入按地址的遥测额度；带了 `Authorization` 头也被忽略，所以没有账号的客户端和仍带令牌的旧客户端都能上报。`crash` 事件另外计入按地址的每日上限。事件和崩溃分组通过单条语句原子写入。
func (a *Service) Telemetry(w http.ResponseWriter, r *http.Request) {
	if a == nil {
		writeError(w, 503, "user_auth_disabled")
		return
	}
	var v telemetryEvent
	if !readSized(w, r, &v, 32768) {
		return
	}
	if v.Kind == "crash" {
		v.Message, v.Stack = telemetryLines(v.Message), truncateStack(telemetryLines(v.Stack), telemetryStackLimit)
	}
	if !v.valid() {
		writeError(w, 400, "invalid_event")
		return
	}
	ctx := r.Context()
	signature := ""
	var releaseCrashRate func() error
	if v.Kind == "crash" {
		// 已记录的 ID 是客户端重试（例如上次的 202 在网络中丢失），直接按重复事件返回 202，不再占用当天的崩溃额度；否则同一条崩溃重试几次就会挤掉其他崩溃。
		var seen bool
		if err := a.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM admin_events WHERE id=$1)`, v.ID).Scan(&seen); err != nil {
			a.error(w, err)
			return
		} else if seen {
			write(w, 202, map[string]bool{"accepted": true})
			return
		}
		refund, _, err := a.ReserveRateLimit(ctx, "telemetry-crash", a.clientAddress(r), telemetryCrashDailyLimit, 24*time.Hour)
		if errors.Is(err, ErrLimited) {
			// 每日窗口不会在一分钟内重开，所以让客户端把排队的崩溃留一小时再发，而不是每分钟重试。
			w.Header().Set("Retry-After", "3600")
			writeError(w, 429, "rate_limit_exceeded")
			return
		} else if err != nil {
			a.error(w, err)
			return
		}
		releaseCrashRate = func() error {
			releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancelRelease()
			return refund(releaseCtx)
		}
		signature = crashSignature(v.Platform, v.Message, v.Stack)
	}
	// 只为本次真正插入的崩溃事件更新分组，避免并发重试刷新或创建分组。
	var inserted bool
	// id 检查和每日额度扣除之间存在并发窗口；若另一个请求先写入，退回本次预留的额度。
	err := a.store.pool.QueryRow(ctx, `WITH inserted AS (
 INSERT INTO admin_events(id,kind,platform,version,message,stack,artifact,channel,install_id,signature)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(id) DO NOTHING
 RETURNING signature,version
	), grouped AS (
	 INSERT INTO admin_crash_groups(signature,platform,version,title)
SELECT signature,$11,version,$12 FROM inserted WHERE signature IS NOT NULL
	ON CONFLICT(signature) DO UPDATE SET first_seen=LEAST(admin_crash_groups.first_seen,EXCLUDED.first_seen),
	 last_seen=GREATEST(admin_crash_groups.last_seen,EXCLUDED.last_seen),
	 version=CASE WHEN EXCLUDED.last_seen>admin_crash_groups.last_seen THEN EXCLUDED.version ELSE admin_crash_groups.version END
	)
	SELECT EXISTS(SELECT 1 FROM inserted)`,
		v.ID, v.Kind, v.Platform, v.Version, v.Message, v.Stack, telemetryNull(v.Artifact), telemetryNull(v.Channel), telemetryNull(v.InstallID), telemetryNull(signature), crashPlatform(v.Platform), crashGroupTitle(v.Message)).Scan(&inserted)
	if err != nil {
		if releaseCrashRate != nil && DatabaseWriteDefinitelyRejected(err) {
			_ = releaseCrashRate()
		}
		a.error(w, err)
		return
	}
	if releaseCrashRate != nil && !inserted {
		if err := releaseCrashRate(); err != nil {
			a.error(w, err)
			return
		}
	}
	write(w, 202, map[string]bool{"accepted": true})
}
