package account

import "github.com/jackc/pgx/v5"

// collectRowMap 返回非 nil 映射，重复键由后读的行覆盖；扫描失败丢弃结果，迭代失败保留已读结果，返回前关闭结果集。
func collectRowMap[K comparable, V any](rows pgx.Rows, scan func(pgx.CollectableRow) (K, V, error)) (map[K]V, error) {
	defer rows.Close()
	out := map[K]V{}
	for rows.Next() {
		key, value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}
