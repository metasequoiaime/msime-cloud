package account

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const emailDeliveryRetentionDays = 180

const (
	emailDeliveryAccepted = "accepted"
	emailDeliveryRejected = "rejected"
	emailDeliveryFailed   = "failed"
)

func (a *Service) recordEmailDelivery(provider, target string, err error) {
	if provider != "email" {
		return
	}
	logEmailDelivery(target, err)
	if a.store == nil {
		return
	}
	// 统计写入使用独立短期限，邮件请求取消或统计表短暂故障都不能改变投递结果。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a.store.RecordEmailDelivery(ctx, emailDeliveryStatus(err))
}

// logEmailDelivery 只记录规范化后的域名和结果，不记录完整邮箱地址或 SMTP 原文。
// 域名能帮助定位供应商按域名拒收的问题，且不会暴露收件人的本地部分。
func logEmailDelivery(target string, err error) {
	domain := "unknown"
	if at := strings.LastIndexByte(target, '@'); at > 0 && at < len(target)-1 {
		domain = strings.ToLower(target[at+1:])
		if strings.ContainsAny(domain, "\r\n") {
			domain = "unknown"
		}
	}
	status := emailDeliveryStatus(err)
	if err == nil {
		slog.Info("邮件投递", "domain", domain, "status", status)
		return
	}
	slog.Warn("邮件投递失败", "domain", domain, "status", status)
}

// RecordEmailDelivery 记录一次已经调用邮件供应商的投递结果。记录失败不能阻断邮件请求，避免统计表故障扩大成登录故障。
func (s *Store) RecordEmailDelivery(ctx context.Context, status string) {
	if status != emailDeliveryAccepted && status != emailDeliveryRejected && status != emailDeliveryFailed {
		slog.Warn("邮件投递统计状态无效", "status", status)
		return
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO admin_email_delivery_daily(day,attempted,accepted,rejected,failed)
VALUES((clock_timestamp() AT TIME ZONE 'UTC')::date,1,CASE WHEN $1='accepted' THEN 1 ELSE 0 END,CASE WHEN $1='rejected' THEN 1 ELSE 0 END,CASE WHEN $1='failed' THEN 1 ELSE 0 END)
ON CONFLICT(day) DO UPDATE SET attempted=admin_email_delivery_daily.attempted+1,
 accepted=admin_email_delivery_daily.accepted+CASE WHEN $1='accepted' THEN 1 ELSE 0 END,
 rejected=admin_email_delivery_daily.rejected+CASE WHEN $1='rejected' THEN 1 ELSE 0 END,
 failed=admin_email_delivery_daily.failed+CASE WHEN $1='failed' THEN 1 ELSE 0 END`, status)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("邮件投递统计写入失败", "reason", err.Error())
	}
}

func emailDeliveryStatus(err error) string {
	if err == nil {
		return emailDeliveryAccepted
	}
	var failure *deliveryFailure
	if errors.As(err, &failure) {
		if failure.permanent {
			return emailDeliveryRejected
		}
	}
	// DATA 阶段失败只说明邮件可能已投递，不能当作供应商已确认接受。
	return emailDeliveryFailed
}
