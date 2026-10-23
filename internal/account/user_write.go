package account

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// scanUserWritePermission 解析调用方锁内查询返回的封禁标记；用户不存在时返回指定错误，其他数据库错误原样返回。
func scanUserWritePermission(row pgx.Row, missingUser error) error {
	var banned bool
	if err := row.Scan(&banned); errors.Is(err, pgx.ErrNoRows) {
		return missingUser
	} else if err != nil {
		return err
	}
	if banned {
		return ErrBanned
	}
	return nil
}
