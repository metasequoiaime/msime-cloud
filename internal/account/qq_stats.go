package account

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// IncrQQOutcome 给 day 这天的 outcome 计数加一。
func (a *Service) IncrQQOutcome(ctx context.Context, day time.Time, outcome string) error {
	_, err := a.store.pool.Exec(ctx, `INSERT INTO qq_daily_stats(day,outcome,count) VALUES($1,$2,1) ON CONFLICT (day,outcome) DO UPDATE SET count=qq_daily_stats.count+1`, day.Format("2006-01-02"), outcome)
	return err
}

// QQOutcomes 返回 from 这天（含）到 to 这天（不含）每种处理结果的条数合计；按日期算，from、to 是统计时区（北京时间）里的日期。
func (a *Service) QQOutcomes(ctx context.Context, from, to time.Time) (map[string]int, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT outcome,sum(count) FROM qq_daily_stats WHERE day>=$1 AND day<$2 GROUP BY outcome`, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	return collectRowMap(rows, func(row pgx.CollectableRow) (string, int, error) {
		var outcome string
		var n int
		err := row.Scan(&outcome, &n)
		return outcome, n, err
	})
}
