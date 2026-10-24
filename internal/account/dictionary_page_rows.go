package account

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// collectDictionaryPage 收齐并关闭结果集，空页复查用户，再用额外一行判断后续；读取失败丢弃整页，成功空页保留非 nil 列表。
func collectDictionaryPage[T any](ctx context.Context, q userReadQuerier, user string, limit int, rows pgx.Rows, scan pgx.RowToFunc[T]) ([]T, bool, error) {
	items, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, false, err
	}
	if len(items) == 0 {
		if err := requireUserExistsQuery(ctx, q, user); err != nil {
			return nil, false, err
		}
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}
