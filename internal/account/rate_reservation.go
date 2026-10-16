package account

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReserveRateLimit 预留一次可退款额度，拒绝时不增加计数并返回剩余等待时间。成功返回的退款函数绑定本窗口，且最多执行一次；调用方只在明确失败时调用，并提供独立的短时上下文。预留期间不持有数据库连接。
func (a *Service) ReserveRateLimit(ctx context.Context, scope, subject string, limit int, window time.Duration) (func(context.Context) error, time.Duration, error) {
	if limit <= 0 || window <= 0 {
		return nil, 0, ErrInvalid
	}
	key := scope + ":" + hash(subject)
	var until time.Time
	err := a.store.pool.QueryRow(ctx, `INSERT INTO auth_rates AS a(key,count,expires_at)
VALUES($1,1,clock_timestamp()+$2::interval)
ON CONFLICT(key) DO UPDATE SET (count,expires_at)=(
 SELECT CASE WHEN a.expires_at<=stamp.now THEN 1 ELSE a.count+1 END,
 CASE WHEN a.expires_at<=stamp.now THEN stamp.now+$2::interval ELSE a.expires_at END
 FROM (SELECT clock_timestamp() AS now) stamp
)
WHERE a.expires_at<=clock_timestamp() OR a.count<$3
RETURNING expires_at`, key, window.String(), limit).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		// 冲突分支没有更新时 RETURNING 不返回行；单独读取数据库时钟，只在拒绝路径多一次往返。
		var count int
		var expires, now time.Time
		err = a.store.pool.QueryRow(ctx, `SELECT count,expires_at,clock_timestamp() FROM auth_rates WHERE key=$1`, key).Scan(&count, &expires, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, time.Second, ErrLimited
		}
		if err != nil {
			return nil, 0, err
		}
		// 两次查询之间可能已有请求退款或窗口重开，出现空位时提示尽快重试。
		if count < limit {
			return nil, time.Second, ErrLimited
		}
		return nil, max(expires.Sub(now), time.Second), ErrLimited
	}
	if err != nil {
		return nil, 0, err
	}
	var once sync.Once
	var releaseErr error
	return func(releaseCtx context.Context) error {
		once.Do(func() {
			_, releaseErr = a.store.pool.Exec(releaseCtx, `WITH locked AS MATERIALIZED (
 SELECT key,expires_at FROM auth_rates WHERE key=$1 FOR UPDATE
)
UPDATE auth_rates a SET count=greatest(a.count-1,0) FROM locked l
WHERE a.key=l.key AND l.expires_at=$2 AND l.expires_at>clock_timestamp()`, key, until)
		})
		return releaseErr
	}, 0, nil
}
