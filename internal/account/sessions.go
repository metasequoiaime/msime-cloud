package account

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mileusna/useragent"
)

// maxListedSessions 是「我的设备」一次列出的会话上限，按最近活跃倒序。
const maxListedSessions = 50

// UserSession 是 GET /v1/users/me/sessions 的一项。platform、name、app_version 由登录时记录的 User-Agent 解析，解析不出来时为空字符串；last_active 由访问令牌的到期时间推算（每次刷新把它推后 15 分钟），和管理后台的算法相同。
type UserSession struct {
	ID         string    `json:"id"`
	Platform   string    `json:"platform"`
	Name       string    `json:"name"`
	AppVersion string    `json:"app_version"`
	CreatedAt  time.Time `json:"created_at"`
	LastActive time.Time `json:"last_active"`
	Current    bool      `json:"current"`
}

// sessionPlatforms 是客户端 User-Agent 里认识的平台 ID，与遥测的规范平台 ID 一致。
var sessionPlatforms = map[string]bool{"android": true, "ios": true, "macos": true, "windows": true, "linux": true, "harmony": true}

// msimeAgent 匹配客户端自报的 `msime-<平台>/<版本> (<设备型号>; <系统>; edition=<版本 ID>)`，括号部分可省略。
var msimeAgent = regexp.MustCompile(`^msime-([a-z]+)/([0-9A-Za-z.+_-]{1,32})(?: \(([^()]*)\))?$`)

// legacyAgent 匹配旧客户端统一发送的 `MSIME/Android` 这类只有平台名的 User-Agent。
var legacyAgent = regexp.MustCompile(`^MSIME/([A-Za-z]+)$`)

// browserSystems 是认得出的浏览器所在系统；浏览器里的会话（官网登录）平台记为 web。
var browserSystems = map[string]bool{useragent.MacOS: true, useragent.IOS: true, useragent.Android: true, useragent.Windows: true, useragent.Linux: true, useragent.ChromeOS: true}

// describeSessionAgent 把登录时记录的 User-Agent 解析成平台、设备名和应用版本。不认识的格式三项都为空，客户端显示为「未知设备」。
func describeSessionAgent(header string) (platform, name, version string) {
	header = strings.TrimSpace(header)
	if m := msimeAgent.FindStringSubmatch(header); m != nil && sessionPlatforms[m[1]] {
		model := ""
		if m[3] != "" {
			model, _, _ = strings.Cut(m[3], ";")
			model = strings.TrimSpace(model)
			if strings.Contains(model, "=") || utf8.RuneCountInString(model) > 64 {
				model = ""
			}
		}
		return m[1], model, m[2]
	}
	if m := legacyAgent.FindStringSubmatch(header); m != nil {
		if p := strings.ToLower(m[1]); sessionPlatforms[p] {
			return p, "", ""
		}
		return "", "", ""
	}
	ua := useragent.Parse(header)
	if !(ua.Desktop || ua.Mobile || ua.Tablet) || ua.Bot || !browserSystems[ua.OS] || ua.Name == "" || strings.ContainsAny(ua.Name, "/-_") {
		return "", "", ""
	}
	where := ua.OS
	if ua.Device != "" && (ua.OS == useragent.IOS || ua.OS == useragent.Android) {
		where = ua.Device
	}
	return "web", ua.Name + " · " + where, ""
}

// sessions 处理 GET /v1/users/me/sessions：列出本人未撤销、未过期的会话，当前请求所用的会话标记 current。匿名账号是另一个用户，不会出现在真实账号的列表里。
func (a *Service) sessions(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	rows, err := a.store.pool.Query(r.Context(), `SELECT id,user_agent,created_at,greatest(created_at,access_expires-interval '15 minutes') AS last_active FROM auth_sessions
 WHERE user_id=$1 AND NOT revoked AND expires_at>now() ORDER BY last_active DESC,id LIMIT $2`, p.UserID, maxListedSessions)
	if err != nil {
		a.error(w, err)
		return
	}
	defer rows.Close()
	out := []UserSession{}
	for rows.Next() {
		var v UserSession
		var agent string
		if err = rows.Scan(&v.ID, &agent, &v.CreatedAt, &v.LastActive); err != nil {
			a.error(w, err)
			return
		}
		v.Platform, v.Name, v.AppVersion = describeSessionAgent(agent)
		v.CreatedAt, v.LastActive = v.CreatedAt.UTC(), v.LastActive.UTC()
		v.Current = v.ID == p.SessionID
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		a.error(w, err)
		return
	}
	write(w, 200, map[string]any{"sessions": out})
}

// sessionID 是会话 ID 的形状：64 位小写十六进制。
var sessionID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// revokeSession 处理 DELETE /v1/users/me/sessions/{id}：撤销本人的一个会话，撤销当前会话等于退出登录。别人的、已撤销或已过期的会话一律 404，不区分是否存在。撤销只会减少访问权限，所以不要求最近登录。
func (a *Service) revokeSession(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !sessionID.MatchString(id) {
		writeError(w, 404, "session_not_found")
		return
	}
	tag, err := a.store.pool.Exec(r.Context(), `UPDATE auth_sessions SET revoked=true WHERE id=$1 AND user_id=$2 AND NOT revoked AND expires_at>now()`, id, p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 404, "session_not_found")
		return
	}
	w.WriteHeader(204)
}
