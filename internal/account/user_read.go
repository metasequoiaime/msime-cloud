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

// communityItemReadTransaction 锁住社区作品作者行，保证详情读取与账号删除按提交顺序完成。table 只接收本包内固定的社区表名。
func (s *Store) communityItemReadTransaction(ctx context.Context, table, id string) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var owner string
	query := `SELECT s.owner_id FROM ` + table + ` s JOIN auth_users u ON u.id=s.owner_id WHERE s.id=$1 FOR SHARE OF u`
	if err := tx.QueryRow(ctx, query, id).Scan(&owner); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
