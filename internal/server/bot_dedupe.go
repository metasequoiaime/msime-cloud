package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqReserveDedupe 占用写入成功后保留的去重名额，返回明确失败时的退款函数。竞争失败不记次，退款只释放本次租约，避免旧请求影响新窗口。
func (s *Server) qqReserveDedupe(ctx context.Context, scope, subject string, window time.Duration) (func(), error) {
	leases, ok := s.limits.(rateLeaseLimiter)
	if !ok {
		return nil, account.ErrInvalid
	}
	until, err := leases.AcquireRateLease(ctx, scope, subject, window)
	if err != nil {
		return nil, err
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := leases.ReleaseRateLease(releaseCtx, scope, subject, until); err != nil {
			slog.Warn("qq bot: dedupe slot not released", "scope", scope, "reason", err.Error())
		}
	}, nil
}
