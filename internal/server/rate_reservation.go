package server

import (
	"context"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// rateReservationLimiter 提供可退款的共享额度，每次成功预留都返回绑定窗口且只执行一次的退款函数。
type rateReservationLimiter interface {
	ReserveRateLimit(context.Context, string, string, int, time.Duration) (func(context.Context) error, time.Duration, error)
}

func (s *Server) reserveRateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) (func(context.Context) error, time.Duration, error) {
	limiter, ok := s.limits.(rateReservationLimiter)
	if !ok {
		return nil, 0, account.ErrInvalid
	}
	return limiter.ReserveRateLimit(ctx, scope, subject, limit, window)
}
