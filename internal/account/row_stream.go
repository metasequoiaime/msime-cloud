package account

import "github.com/jackc/pgx/v5"

// streamQueryRows 逐行扫描并立即输出，不预先收集；返回是否读到过行，保留原始错误，并在返回前关闭结果集。
func streamQueryRows[T any](rows pgx.Rows, scan pgx.RowToFunc[T], emit func(T) error) (bool, error) {
	defer rows.Close()
	seen := false
	for rows.Next() {
		seen = true
		value, err := scan(rows)
		if err != nil {
			return seen, err
		}
		if err := emit(value); err != nil {
			return seen, err
		}
	}
	return seen, rows.Err()
}
