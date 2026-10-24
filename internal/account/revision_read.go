package account

import (
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// readRevisionJSON 读取版本与 JSON；缺失行保留调用方的默认值，解码失败仍保留版本和已解码部分。
func readRevisionJSON(row pgx.Row, revision *int64, value any) error {
	var raw []byte
	err := row.Scan(revision, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
