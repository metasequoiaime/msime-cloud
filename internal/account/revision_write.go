package account

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

var errRevisionConflict = errors.New("revision_conflict")

// checkUserDataRevision 在已持有用户行锁的事务中读取词库版本，并校验调用方带来的版本。
func checkUserDataRevision(ctx context.Context, tx pgx.Tx, user string, expected int64) (int64, error) {
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM user_dictionary_state WHERE user_id=$1),0)`, user).Scan(&revision); err != nil {
		return 0, err
	}
	if revision != expected {
		return 0, errRevisionConflict
	}
	return revision, nil
}

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
