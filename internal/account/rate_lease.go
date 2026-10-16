package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AcquireRateLease 原子占用共享租约，返回本次截止时间作为释放标识。已被占用时返回 ErrLimited，不改变计数，避免多个竞争者退款提前放开持有者。
func (a *Service) AcquireRateLease(ctx context.Context, scope, subject string, window time.Duration) (time.Time, error) {
	var until time.Time
	err := a.store.pool.QueryRow(ctx, `INSERT INTO auth_rates(key,count,expires_at)
VALUES($1,1,clock_timestamp()+$2::interval)
ON CONFLICT(key) DO UPDATE SET count=1,expires_at=clock_timestamp()+$2::interval
WHERE auth_rates.count=0 OR auth_rates.expires_at<=clock_timestamp()
RETURNING expires_at`, scope+":"+hash(subject), window.String()).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrLimited
	}
	return until, err
}

// ReleaseRateLease 只释放仍属于本次占用的租约；旧请求延迟收尾或重复释放时不影响后来的持有者。
func (a *Service) ReleaseRateLease(ctx context.Context, scope, subject string, until time.Time) error {
	_, err := a.store.pool.Exec(ctx, `UPDATE auth_rates SET count=0 WHERE key=$1 AND expires_at=$2`, scope+":"+hash(subject), until)
	return err
}
