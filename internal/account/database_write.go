package account

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// DatabaseWriteDefinitelyRejected 判断写入错误是否属于 PostgreSQL 明确返回的错误。
// 仅这种错误允许退还写入额度；连接中断、超时或取消的结果可能未知。
func DatabaseWriteDefinitelyRejected(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr)
}
