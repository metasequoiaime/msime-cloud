package account

import (
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// emitDictionarySnapshotRows 按查询顺序输出快照并关闭结果流；空结果时保留用户不存在语义。
func emitDictionarySnapshotRows(rows pgx.Rows, emit func(json.RawMessage) error) error {
	defer rows.Close()
	seen := false
	for rows.Next() {
		seen = true
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if err := emit(raw); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !seen {
		return errMissingUser
	}
	return nil
}
