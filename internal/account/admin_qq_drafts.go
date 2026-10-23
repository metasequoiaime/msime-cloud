package account

import (
	"context"
	"fmt"
	"time"
)

// AdminEditQQDraft 将管理员编辑与审计放在同一事务中；时间戳和内容快照共同避免覆盖并发补充。
func (a *Service) AdminEditQQDraft(ctx context.Context, previous QQDraft, original, report string) (time.Time, error) {
	if err := checkPerm(ctx, PermTriageIssues); err != nil {
		return time.Time{}, err
	}
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var stamp *time.Time
	err = tx.QueryRow(ctx, `WITH locked AS MATERIALIZED (
SELECT id,original,report,updated_at,created_at,filed_at,issue_ref FROM qq_drafts WHERE id=$1 FOR UPDATE
), updated AS (
UPDATE qq_drafts SET original=$2,report=$3,updated_at=clock_timestamp()
WHERE id IN (SELECT id FROM locked WHERE `+qqDraftUnclaimed+` AND created_at>clock_timestamp()-$4::interval AND original=$5 AND report=$6 AND updated_at=$7)
RETURNING updated_at
) SELECT (SELECT updated_at FROM updated)`, previous.ID, original, report, QQDraftWindow.String(), previous.Original, previous.Report, previous.UpdatedAt).Scan(&stamp)
	if err != nil {
		return time.Time{}, err
	}
	if stamp == nil {
		return time.Time{}, ErrQQDraftChanged
	}
	if err := auditTx(ctx, tx, "qq_draft_edit", fmt.Sprintf("D%d", previous.ID), nil); err != nil {
		return time.Time{}, err
	}
	return *stamp, tx.Commit(ctx)
}

// AdminCloseQQDraft 沿用忽略草稿的状态和租约条件，与审计一同提交；重复关闭不重复记审计。
func (a *Service) AdminCloseQQDraft(ctx context.Context, id int64) (bool, error) {
	if err := checkPerm(ctx, PermTriageIssues); err != nil {
		return false, err
	}
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `WITH locked AS MATERIALIZED (
SELECT id,created_at,filed_at,issue_ref FROM qq_drafts WHERE id=$1 FOR UPDATE
)
UPDATE qq_drafts SET filed_at=clock_timestamp(),issue_ref=$3
WHERE id IN (SELECT id FROM locked WHERE `+qqDraftUnclaimed+` AND created_at>clock_timestamp()-$2::interval)`, id, QQDraftWindow.String(), QQDraftIgnored)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if err := auditTx(ctx, tx, "qq_draft_close", fmt.Sprintf("D%d", id), nil); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
