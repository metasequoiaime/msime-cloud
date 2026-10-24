package account

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type userReadQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// userReadTransaction 在读取本人数据期间锁住用户行，使账号删除与完整读取按先后顺序提交。
func (s *Store) userReadTransaction(ctx context.Context, user string) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var present int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM auth_users WHERE id=$1 FOR SHARE`, user).Scan(&present); errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return nil, errMissingUser
	} else if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
