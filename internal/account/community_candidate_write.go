package account

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// commitCandidateSkin 先读取写入结果再提交事务；读取失败时由调用方按原流程回滚。
func commitCandidateSkin(ctx context.Context, tx pgx.Tx, userID, id string) (CommunityCandidateSkin, error) {
	item, err := scanCandidateSkin(tx.QueryRow(ctx, candidateSkinSelect(false)+`WHERE s.id=$2`, userID, id))
	if err == nil {
		err = tx.Commit(ctx)
	}
	return item, err
}
