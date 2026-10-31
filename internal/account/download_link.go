package account

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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

// downloadLinkCommitTimeout 是邮件供应商确认成功后完成数据库提交的最长时间；提交结果不能因客户端断开而变成未知。
const downloadLinkCommitTimeout = 5 * time.Second

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func verifiedEmailQuery(ctx context.Context, q queryRower, user string) (string, error) {
	var email string
	err := q.QueryRow(ctx, `SELECT lower(CASE WHEN provider='email' THEN subject ELSE email END)
 FROM auth_identities WHERE user_id=$1 AND (provider='email' OR (provider='google' AND email_verified AND email<>''))
 ORDER BY CASE WHEN provider='email' THEN 0 ELSE 1 END,created_at LIMIT 1`, user).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return email, err
}

// verifiedEmail 返回账号已验证的邮箱：邮箱登录身份的 subject 优先（登录时已经验证过），其次是 email_verified 的 Google 邮箱。没有时返回空字符串。
func (s *Store) verifiedEmail(ctx context.Context, user string) (string, error) {
	return verifiedEmailQuery(ctx, s.pool, user)
}

// maskEmail 只保留本地部分的第一个字符和完整域名，例如 u***@example.com。
func maskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	first, _ := utf8.DecodeRuneInString(email[:at])
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
		if err := a.store.requireUserExists(r.Context(), p.UserID); err != nil {
			a.error(w, err)
			return
		}
		writeError(w, 409, "no_verified_email")
		return
	}
	suppressed, err := a.store.DeliverySuppressed(r.Context(), "email", email)
	if err != nil {
		a.error(w, err)
		return
	}
	if suppressed {
		writeError(w, 503, "delivery_failed")
		return
	}
	for _, lim := range downloadLinkLimits {
		if err = a.store.Rate(r.Context(), lim.key+":"+hash(p.UserID), lim.n, lim.window); err != nil {
			a.error(w, err)
			return
		}
	}
	releaseGlobal, _, err := a.reserveRateKey(r.Context(), "delivery-global", deliveryGlobalDailyLimit, 24*time.Hour)
	if err != nil {
		a.error(w, err)
		return
	}
	// 限流在事务前独立计数，避免持有连接时再申请同一连接池；发送失败仍消耗额度。
	var tx pgx.Tx
	var deliveryErr error
	attempted, refundGlobal := false, false
	rollback := func() {
		if tx == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}
	defer func() {
		// 先释放用户事务的连接，再写统计、抑制记录或退款，确保单连接池也能完成请求。
		rollback()
		if attempted {
			a.recordEmailDelivery("email", email, deliveryErr)
			if permanentDeliveryFailure(deliveryErr) {
				a.suppressDeliveryTarget(r.Context(), "email", email)
			}
		}
		if refundGlobal {
			a.refundDeliveryReservation(r.Context(), releaseGlobal)
		}
	}()
	tx, err = a.store.userDataTransaction(r.Context(), p.UserID)
	if err != nil {
		refundGlobal = true
		a.error(w, err)
		return
	}
	// 发信前在用户锁内重新读取邮箱，保证等待期间提交的封禁或身份变更生效。
	email, err = verifiedEmailQuery(r.Context(), tx, p.UserID)
	if err != nil {
		refundGlobal = true
		a.error(w, err)
		return
	}
	if email == "" {
		refundGlobal = true
		writeError(w, 409, "no_verified_email")
		return
	}
	suppressed, err = deliverySuppressedQuery(r.Context(), tx, "email", email)
	if err != nil {
		refundGlobal = true
		a.error(w, err)
		return
	}
	if suppressed {
		refundGlobal = true
		writeError(w, 503, "delivery_failed")
		return
	}
	subject, body := downloadLinkMessage(v.Platform)
	err = a.mailer.Mail(r.Context(), email, subject, body)
	attempted, deliveryErr = true, err
	if err != nil {
		refundGlobal = deliveryMayRefund(err)
		writeError(w, 503, "delivery_failed")
		return
	}
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(r.Context()), downloadLinkCommitTimeout)
	err = tx.Commit(commitCtx)
	cancelCommit()
	if err != nil {
		a.error(w, err)
		return
	}
	write(w, 202, map[string]any{"sent_to": maskEmail(email)})
}
