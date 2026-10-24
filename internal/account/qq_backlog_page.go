package account

import "github.com/jackc/pgx/v5"

func collectQQBacklogPage[T any](rows pgx.Rows, offset int, scan func(pgx.CollectableRow, *int) (T, error), count func(*int) error) ([]T, int, error) {
	total := 0
	items, err := pgx.AppendRows([]T(nil), rows, func(row pgx.CollectableRow) (T, error) {
		return scan(row, &total)
	})
	if err != nil {
		return nil, 0, err
	}
	if len(items) == 0 && offset > 0 {
		err = count(&total)
	}
	return items, total, err
}
