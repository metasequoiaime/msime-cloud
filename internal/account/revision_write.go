package account

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

var errRevisionConflict = errors.New("revision_conflict")

// writeRevisionJSON 在已持有用户行锁的事务中编码 JSON 并执行版本条件写入。
// query 由调用方提供固定 SQL；结果构造和事务提交仍由调用方负责。
func writeRevisionJSON(ctx context.Context, tx pgx.Tx, query, user string, expected int64, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, query, user, expected, expected+1, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errRevisionConflict
	}
	return nil
}
