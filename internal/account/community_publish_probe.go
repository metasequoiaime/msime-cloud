package account

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var errCommunityPublishConflict = errors.New("community publish id conflict")

// communityPublishedItem 在同一快照中检查作者、请求摘要并读取重试响应；冲突时跳过响应聚合。
// table 和 itemQuery 都由各发布处理函数提供固定 SQL，用户输入只通过绑定参数传入。
// itemQuery 用 publish_match 的 id 约束主键，使响应聚合依赖已通过校验的条目。
func (s *Store) communityPublishedItem(ctx context.Context, table, itemQuery, user, id, digest string) (pgx.Row, error) {
	rows, err := s.pool.Query(ctx, `SELECT existing.owner_id,existing.request_sha256,item.*
FROM `+pgx.Identifier{table}.Sanitize()+` existing
LEFT JOIN LATERAL (
 WITH publish_match AS MATERIALIZED (
  SELECT existing.id WHERE existing.owner_id=$1 AND existing.request_sha256=$3
 )
 `+itemQuery+`) item ON true
WHERE existing.id=$2`, user, id, digest)
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		err := rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		return nil, pgx.ErrNoRows
	}
	var owner, request string
	var itemID *string
	fields := make([]any, len(rows.FieldDescriptions()))
	fields[0], fields[1], fields[2] = &owner, &request, &itemID
	if err := rows.Scan(fields...); err != nil {
		rows.Close()
		return nil, err
	}
	if owner != user || request != digest || itemID == nil {
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if owner != user || request != digest {
			return nil, errCommunityPublishConflict
		}
		return nil, errors.New("published community item missing")
	}
	return communityPublishedItemRow{rows}, nil
}

// communityPublishedItemRow 复用已有条目扫描器，跳过已校验的两列；Scan 与 QueryRow 一样负责关闭结果。
type communityPublishedItemRow struct{ pgx.Rows }

func (row communityPublishedItemRow) Scan(dest ...any) error {
	fields := make([]any, len(dest)+2)
	copy(fields[2:], dest)
	err := row.Rows.Scan(fields...)
	row.Rows.Close()
	if err != nil {
		return err
	}
	return row.Rows.Err()
}
