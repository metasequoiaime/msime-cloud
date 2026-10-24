package account

import (
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// emitDictionarySnapshotRows 按查询顺序输出快照并关闭结果流；空结果时保留用户不存在语义。
func emitDictionarySnapshotRows(rows pgx.Rows, emit func(json.RawMessage) error) error {
	seen, err := streamQueryRows(rows, func(row pgx.CollectableRow) (json.RawMessage, error) {
		var raw []byte
		err := row.Scan(&raw)
		return json.RawMessage(raw), err
	}, emit)
	if err != nil {
		return err
	}
	if !seen {
		return errMissingUser
	}
	return nil
}
