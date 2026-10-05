package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// downloadLinkPlatforms 是 POST /v1/users/me/download-link 认可的平台，也是官网下载页 release 参数的取值。
var downloadLinkPlatforms = map[string]string{
	"windows":    "Windows",
	"macos":      "macOS",
	"linux":      "Linux",
	"harmony-pc": "鸿蒙电脑",
	"ios":        "iOS",
	"android":    "Android",
	"harmony":    "鸿蒙",
}

// downloadLinkLimits 是每个用户发送下载链接的额度；全站另外计入验证码共用的 delivery-global。
var downloadLinkLimits = []struct {
	key    string
	n      int
	window time.Duration
}{{"download-link-hour", 3, time.Hour}, {"download-link-day", 10, 24 * time.Hour}}

// verifiedEmail 返回账号已验证的邮箱：邮箱登录身份的 subject 优先（登录时已经验证过），其次是 email_verified 的 Google 邮箱。没有时返回空字符串。
func (s *Store) verifiedEmail(ctx context.Context, user string) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `SELECT email FROM (
 SELECT 0 AS o,subject AS email,created_at FROM auth_identities WHERE user_id=$1 AND provider='email'
 UNION ALL SELECT 1,email,created_at FROM auth_identities WHERE user_id=$1 AND provider='google' AND email_verified AND email<>''
) e ORDER BY o,created_at LIMIT 1`, user).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return email, err
}

// maskEmail 只保留本地部分的第一个字符和完整域名，例如 u***@example.com。
func maskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	first := []rune(email[:at])[0]
	return string(first) + "***" + email[at:]
}

// downloadLinkMessage 是下载链接邮件的固定模板。链接只由白名单里的平台 ID 拼出，不含任何用户输入。
func downloadLinkMessage(platform string) (subject, body string) {
	link := "https://msime.app/download/?release=" + platform
	return "水杉输入法 " + downloadLinkPlatforms[platform] + " 版下载链接", "你在水杉输入法里请求了 " + downloadLinkPlatforms[platform] + " 版的下载链接：\r\n\r\n" + link + "\r\n\r\n如果不是你本人操作，可以忽略这封邮件。"
}

// downloadLink 处理 POST /v1/users/me/download-link {"platform"}：把固定模板的下载链接发到账号已验证的邮箱，返回 202 {sent_to}（打码）。不接受收件地址，避免成为垃圾邮件中继；没有已验证邮箱返回 409 no_verified_email。
func (a *Service) downloadLink(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	var v struct {
		Platform string `json:"platform"`
	}
	if !read(w, r, &v) {
		return
	}
	if _, known := downloadLinkPlatforms[v.Platform]; !known {
		writeError(w, 400, "invalid_platform")
		return
	}
	if a.config.Email.From == "" {
		writeError(w, 503, "provider_disabled")
		return
	}
	email, err := a.store.verifiedEmail(r.Context(), p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	if email == "" {
		writeError(w, 409, "no_verified_email")
		return
	}
	for _, lim := range downloadLinkLimits {
		if err = a.store.Rate(r.Context(), lim.key+":"+hash(p.UserID), lim.n, lim.window); err != nil {
			a.error(w, err)
			return
		}
	}
	if err = a.store.Rate(r.Context(), "delivery-global", 500, 24*time.Hour); err != nil {
		a.error(w, err)
		return
	}
	subject, body := downloadLinkMessage(v.Platform)
	if err = a.mailer.Mail(r.Context(), email, subject, body); err != nil {
		writeError(w, 503, "delivery_failed")
		return
	}
	write(w, 202, map[string]any{"sent_to": maskEmail(email)})
}
