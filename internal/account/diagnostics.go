package account

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// DiagnosticsMCPPrefix 是诊断快照远程 MCP 端点的路径前缀，完整路径是 /mcp/s/{id}。它不在 /v1 下，不用用户会话而用快照令牌鉴权，IsPath 让它绕过全局 Bearer 检查。
const DiagnosticsMCPPrefix = "/mcp/s/"

// diagnosticsMCPURL 是返回给客户端的 MCP 地址前缀，产品只用 api.msime.app，不另配域名。
const diagnosticsMCPURL = "https://api.msime.app" + DiagnosticsMCPPrefix

const (
	// diagnosticsBodyBytes 是一次上传的请求体上限。
	diagnosticsBodyBytes = 2 << 20
	// diagnosticsUploadsPerHour 是每个用户每小时能上传的次数（auth_rates，所有副本共享）。
	diagnosticsUploadsPerHour = 6
	// diagnosticsTokensPerHour 是每个用户每小时能重新生成令牌的次数。
	diagnosticsTokensPerHour = 20
	// diagnosticAccessArgumentBytes 是访问记录里 arguments 的上限，超过时截断成字符串。
	diagnosticAccessArgumentBytes = 1024
	// diagnosticAccessListLimit 是 GET 返回的最近访问条数。
	diagnosticAccessListLimit = 50
	// DiagnosticRedacted 是 config_snapshot 里凭据类键必须使用的值。
	DiagnosticRedacted = "<redacted>"

	diagnosticCrashLimit        = 200
	diagnosticEventLimit        = 50000
	diagnosticCrashMessageBytes = 2 << 10
	diagnosticCrashStackBytes   = 16 << 10
	diagnosticConfigDepth       = 8
	diagnosticConfigKeyBytes    = 128
	diagnosticConfigStringBytes = 1024
	diagnosticConfigEntries     = 2000
)

// DiagnosticEventKinds 是 input_events 与 perf_trace 唯一认可的事件种类（plan P19）。事件只有时间戳、种类和耗时，永远不含文本、拼音、候选或按键字符。
var DiagnosticEventKinds = map[string]bool{"key_down": true, "key_up": true, "candidate_shown": true, "candidate_selected": true, "commit": true, "backspace": true, "panel_open": true, "panel_close": true, "ime_start": true, "ime_finish": true}

// diagnosticTTLs 是上传可选的保存时长。
var diagnosticTTLs = map[string]time.Duration{"one_hour": time.Hour, "one_day": 24 * time.Hour, "seven_days": 7 * 24 * time.Hour}

// diagnosticCredentialKey 匹配 config_snapshot 里必须已脱敏的键名。
var diagnosticCredentialKey = regexp.MustCompile(`(?i)token|secret|password|api_key|key$`)

var diagnosticIDPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)
var diagnosticLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// ValidDiagnosticID 判断 id 是否是快照 ID 的格式（24 位小写十六进制）。
func ValidDiagnosticID(id string) bool { return diagnosticIDPattern.MatchString(id) }

// DiagnosticEvent 是 input_events / perf_trace 的一条记录。perf_trace 要求 duration_ms，input_events 可以省略。
type DiagnosticEvent struct {
	TMS        int64  `json:"t_ms"`
	Kind       string `json:"kind"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
}

// DiagnosticCrash 是一条崩溃记录。
type DiagnosticCrash struct {
	At      string `json:"at"`
	Message string `json:"message"`
	Stack   string `json:"stack"`
}

// DiagnosticSections 是快照内容，字段为 nil 表示用户没有勾选该分类。
type DiagnosticSections struct {
	CrashLogs      *[]DiagnosticCrash `json:"crash_logs,omitempty"`
	PerfTrace      *[]DiagnosticEvent `json:"perf_trace,omitempty"`
	ConfigSnapshot map[string]any     `json:"config_snapshot"`
	InputEvents    *[]DiagnosticEvent `json:"input_events,omitempty"`
}

// Names 按固定顺序列出快照包含的分类。
func (s DiagnosticSections) Names() []string {
	names := []string{}
	if s.CrashLogs != nil {
		names = append(names, "crash_logs")
	}
	if s.PerfTrace != nil {
		names = append(names, "perf_trace")
	}
	if s.ConfigSnapshot != nil {
		names = append(names, "config_snapshot")
	}
	if s.InputEvents != nil {
		names = append(names, "input_events")
	}
	return names
}

// DiagnosticSnapshot 是 MCP 端点读到的一份有效快照。
type DiagnosticSnapshot struct {
	ID, Platform, AppVersion string
	CreatedAt, ExpiresAt     time.Time
	Sections                 DiagnosticSections
}

type diagnosticUpload struct {
	Platform   string             `json:"platform"`
	AppVersion string             `json:"app_version"`
	Sections   DiagnosticSections `json:"sections"`
	TTL        string             `json:"ttl"`
}

// validateDiagnosticUpload 校验整份上传，任何一处不合规都拒绝整份。返回值是错误码，空字符串表示通过。
func validateDiagnosticUpload(v diagnosticUpload) string {
	if !diagnosticLabelPattern.MatchString(v.Platform) || !diagnosticLabelPattern.MatchString(v.AppVersion) {
		return "invalid_client"
	}
	if _, ok := diagnosticTTLs[v.TTL]; !ok {
		return "invalid_ttl"
	}
	s := v.Sections
	if len(s.Names()) == 0 {
		return "empty_snapshot"
	}
	if s.CrashLogs != nil {
		if len(*s.CrashLogs) > diagnosticCrashLimit {
			return "invalid_crash_logs"
		}
		for _, c := range *s.CrashLogs {
			if _, err := time.Parse(time.RFC3339, c.At); err != nil || len(c.At) > 64 || len(c.Message) > diagnosticCrashMessageBytes || len(c.Stack) > diagnosticCrashStackBytes || !utf8.ValidString(c.Message) || !utf8.ValidString(c.Stack) {
				return "invalid_crash_logs"
			}
		}
	}
	if s.PerfTrace != nil && !validDiagnosticEvents(*s.PerfTrace, true) {
		return "invalid_perf_trace"
	}
	if s.InputEvents != nil && !validDiagnosticEvents(*s.InputEvents, false) {
		return "invalid_input_events"
	}
	if s.ConfigSnapshot != nil {
		entries := 0
		if code := validateDiagnosticConfig(s.ConfigSnapshot, 1, &entries); code != "" {
			return code
		}
	}
	return ""
}

func validDiagnosticEvents(events []DiagnosticEvent, durationRequired bool) bool {
	if len(events) > diagnosticEventLimit {
		return false
	}
	for _, e := range events {
		if !DiagnosticEventKinds[e.Kind] || e.TMS < 0 || (durationRequired && e.DurationMS == nil) || (e.DurationMS != nil && *e.DurationMS < 0) {
			return false
		}
	}
	return true
}

// validateDiagnosticConfig 递归检查配置快照：键名匹配凭据模式的值必须是 "<redacted>"，字符串、键名、层数和总条目数都有上限。
func validateDiagnosticConfig(value any, depth int, entries *int) string {
	if depth > diagnosticConfigDepth {
		return "invalid_config_snapshot"
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			*entries++
			if *entries > diagnosticConfigEntries || len(key) > diagnosticConfigKeyBytes {
				return "invalid_config_snapshot"
			}
			if diagnosticCredentialKey.MatchString(key) {
				if child != DiagnosticRedacted {
					return "unredacted_config"
				}
				continue
			}
			if code := validateDiagnosticConfig(child, depth+1, entries); code != "" {
				return code
			}
		}
	case []any:
		for _, child := range v {
			*entries++
			if *entries > diagnosticConfigEntries {
				return "invalid_config_snapshot"
			}
			if code := validateDiagnosticConfig(child, depth+1, entries); code != "" {
				return code
			}
		}
	case string:
		if len(v) > diagnosticConfigStringBytes {
			return "invalid_config_snapshot"
		}
	}
	return ""
}

// newDiagnosticToken 返回 msk_ 加 43 位 base64url（32 字节随机数）的令牌和它的 SHA-256。
func newDiagnosticToken() (token, digest string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = "msk_" + base64.RawURLEncoding.EncodeToString(b)
	return token, hash(token)
}

func newDiagnosticID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func diagnosticTokenHint(token string) string { return token[len(token)-4:] }

// diagnosticsUpload 处理 POST /v1/users/me/diagnostics：校验后替换本人原有的快照，返回只出现这一次的令牌。匿名账号也可以上传。
func (a *Service) diagnosticsUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v diagnosticUpload
	if !readSized(w, r, &v, diagnosticsBodyBytes) {
		return
	}
	if code := validateDiagnosticUpload(v); code != "" {
		writeError(w, 400, code)
		return
	}
	if err := a.store.Rate(r.Context(), "diagnostics-upload:"+hash(p.UserID), diagnosticsUploadsPerHour, time.Hour); err != nil {
		a.error(w, err)
		return
	}
	content, err := json.Marshal(v.Sections)
	if err != nil {
		writeError(w, 400, "invalid_json")
		return
	}
	token, digest := newDiagnosticToken()
	id := newDiagnosticID()
	expires := time.Now().Add(diagnosticTTLs[v.TTL]).UTC().Truncate(time.Second)
	tx, err := a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `DELETE FROM diagnostic_snapshots WHERE user_id=$1`, p.UserID); err != nil {
		a.error(w, err)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO diagnostic_snapshots(id,user_id,token_hash,token_hint,platform,app_version,sections,content,bytes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, p.UserID, digest, diagnosticTokenHint(token), v.Platform, v.AppVersion, v.Sections.Names(), content, len(content), expires); err != nil {
		a.error(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.error(w, err)
		return
	}
	write(w, 201, map[string]any{"id": id, "mcp_url": diagnosticsMCPURL + id, "token": token, "expires_at": expires})
}

type diagnosticAccess struct {
	At          time.Time       `json:"at"`
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
	ResultCount int             `json:"result_count"`
	Bytes       int             `json:"bytes"`
}

// diagnosticsGet 处理 GET /v1/users/me/diagnostics：本人有效快照的元数据（不含内容和令牌）与最近的访问记录。
func (a *Service) diagnosticsGet(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var snapshot struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
		Bytes     int       `json:"bytes"`
		Sections  []string  `json:"sections"`
		TokenHint string    `json:"token_hint"`
	}
	err := a.store.pool.QueryRow(r.Context(), `SELECT id,created_at,expires_at,bytes,sections,token_hint FROM diagnostic_snapshots WHERE user_id=$1 AND expires_at>now()`, p.UserID).Scan(&snapshot.ID, &snapshot.CreatedAt, &snapshot.ExpiresAt, &snapshot.Bytes, &snapshot.Sections, &snapshot.TokenHint)
	if errors.Is(err, pgx.ErrNoRows) {
		write(w, 200, map[string]any{"snapshot": nil, "accesses": []diagnosticAccess{}})
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	rows, err := a.store.pool.Query(r.Context(), `SELECT at,tool,arguments,result_count,bytes FROM diagnostic_accesses WHERE snapshot_id=$1 ORDER BY at DESC,id DESC LIMIT $2`, snapshot.ID, diagnosticAccessListLimit)
	if err != nil {
		a.error(w, err)
		return
	}
	accesses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (diagnosticAccess, error) {
		var v diagnosticAccess
		var arguments []byte
		err := row.Scan(&v.At, &v.Tool, &arguments, &v.ResultCount, &v.Bytes)
		v.Arguments = arguments
		return v, err
	})
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"snapshot": snapshot, "accesses": accesses})
}

// diagnosticsToken 处理 POST /v1/users/me/diagnostics/token：给有效快照换一个新令牌，旧令牌立即失效。
func (a *Service) diagnosticsToken(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if err := a.store.Rate(r.Context(), "diagnostics-token:"+hash(p.UserID), diagnosticsTokensPerHour, time.Hour); err != nil {
		a.error(w, err)
		return
	}
	token, digest := newDiagnosticToken()
	tag, err := a.store.pool.Exec(r.Context(), `UPDATE diagnostic_snapshots SET token_hash=$2,token_hint=$3 WHERE user_id=$1 AND expires_at>now()`, p.UserID, digest, diagnosticTokenHint(token))
	if err != nil {
		a.error(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "diagnostics_not_found")
		return
	}
	write(w, 200, map[string]string{"token": token})
}

// diagnosticsDelete 处理 DELETE /v1/users/me/diagnostics：删除本人的快照和它的访问记录；没有快照也返回 204。
func (a *Service) diagnosticsDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	if _, err := a.store.pool.Exec(r.Context(), `DELETE FROM diagnostic_snapshots WHERE user_id=$1`, p.UserID); err != nil {
		a.error(w, err)
		return
	}
	w.WriteHeader(204)
}

// OpenDiagnosticSnapshot 用 MCP 请求带的令牌打开一份未过期的快照。ID 不存在、已过期、账号被封禁或令牌不符都返回 ErrInvalid，不区分原因；令牌按 SHA-256 常量时间比较。
func (a *Service) OpenDiagnosticSnapshot(ctx context.Context, id, token string) (DiagnosticSnapshot, error) {
	v := DiagnosticSnapshot{ID: id}
	if !ValidDiagnosticID(id) || !strings.HasPrefix(token, "msk_") {
		return v, ErrInvalid
	}
	var stored string
	var content []byte
	err := a.store.pool.QueryRow(ctx, `SELECT d.token_hash,d.platform,d.app_version,d.created_at,d.expires_at,d.content FROM diagnostic_snapshots d JOIN auth_users u ON u.id=d.user_id AND u.banned_at IS NULL WHERE d.id=$1 AND d.expires_at>now()`, id).Scan(&stored, &v.Platform, &v.AppVersion, &v.CreatedAt, &v.ExpiresAt, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrInvalid
	}
	if err != nil {
		return v, err
	}
	supplied := hash(token)
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(stored)) != 1 {
		return v, ErrInvalid
	}
	if err = json.Unmarshal(content, &v.Sections); err != nil {
		return v, err
	}
	return v, nil
}

// RecordDiagnosticAccess 为一次 MCP 工具调用写访问记录。arguments 超过 1 KiB 或不是合法 JSON 时截断成 JSON 字符串保存。
func (a *Service) RecordDiagnosticAccess(ctx context.Context, id, tool string, arguments json.RawMessage, resultCount, bytes int) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO diagnostic_accesses(snapshot_id,tool,arguments,result_count,bytes) VALUES($1,$2,$3,$4,$5)`, id, tool, []byte(boundedDiagnosticArguments(arguments)), resultCount, bytes)
	return err
}

func boundedDiagnosticArguments(arguments json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(arguments))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`)
	}
	if len(trimmed) <= diagnosticAccessArgumentBytes && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	cut := trimmed[:min(len(trimmed), diagnosticAccessArgumentBytes)]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	quoted, _ := json.Marshal(cut)
	// 转义可能让字符串变长，继续缩短直到不超过上限。
	for len(quoted) > diagnosticAccessArgumentBytes {
		cut = cut[:len(cut)-1]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		quoted, _ = json.Marshal(cut)
	}
	return quoted
}
