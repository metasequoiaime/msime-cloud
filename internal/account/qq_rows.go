package account

import "github.com/jackc/pgx/v5"

// collectQQRows 保留空结果的 nil 切片；扫描失败丢弃结果，迭代失败保留此前已读记录，并在返回前释放连接。
func collectQQRows[T any](rows pgx.Rows, scan pgx.RowToFunc[T]) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}
