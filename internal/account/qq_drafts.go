package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// QQDraftWindow 是草稿的有效期：更早的不再列出、不能提交，并由 Store.Prune 删除。
const QQDraftWindow = 30 * 24 * time.Hour

// ErrQQDraftNotFound 表示草稿不存在或已经过期。
var ErrQQDraftNotFound = errors.New("qq draft not found")

// QQDraft 是一份 issue 草稿。Subject 是群和成员标识的摘要。
type QQDraft struct {
	ID        int64
	Subject   string
	Original  string
	Report    string
	CreatedAt time.Time
	Filed     bool
	IssueRef  string
}

// CreateQQDraft 新建一份草稿，subject 由调用方拼成，这里只存摘要。
func (a *Service) CreateQQDraft(ctx context.Context, subject, original, report string) (int64, error) {
	var id int64
	err := a.store.pool.QueryRow(ctx, `INSERT INTO qq_drafts(subject,original,report) VALUES($1,$2,$3) RETURNING id`, QQDraftSubject(subject), original, report).Scan(&id)
	return id, err
}

// QQDraft 读一份有效期内的草稿。
func (a *Service) QQDraft(ctx context.Context, id int64) (QQDraft, error) {
	var d QQDraft
	err := a.store.pool.QueryRow(ctx, `SELECT id,subject,original,report,created_at,filed_at IS NOT NULL,issue_ref FROM qq_drafts WHERE id=$1 AND created_at>now()-$2::interval`, id, QQDraftWindow.String()).Scan(&d.ID, &d.Subject, &d.Original, &d.Report, &d.CreatedAt, &d.Filed, &d.IssueRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrQQDraftNotFound
	}
	return d, err
}

// QQDraftSubject 是草稿里存的 subject 摘要。
func QQDraftSubject(subject string) string { return hash(subject) }

// QQDraftOwnedBy 判断草稿是不是 subject 的。
func QQDraftOwnedBy(d QQDraft, subject string) bool { return d.Subject == QQDraftSubject(subject) }

// UpdateQQDraft 更新还没提交的草稿的原话和整理结果，返回是否更新了。
func (a *Service) UpdateQQDraft(ctx context.Context, id int64, original, report string) (bool, error) {
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_drafts SET original=$2,report=$3,updated_at=now() WHERE id=$1 AND filed_at IS NULL AND created_at>now()-$4::interval`, id, original, report, QQDraftWindow.String())
	return tag.RowsAffected() == 1, err
}

// ClaimQQDraft 认领一份还没提交的草稿准备提交，返回是否认领到：两个副本同时收到「提交」时只有一个能认领，不会提两遍。
func (a *Service) ClaimQQDraft(ctx context.Context, id int64) (bool, error) {
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=now() WHERE id=$1 AND filed_at IS NULL AND created_at>now()-$2::interval`, id, QQDraftWindow.String())
	return tag.RowsAffected() == 1, err
}

// ReleaseQQDraft 在提交失败时放回认领，之后可以再提交。
func (a *Service) ReleaseQQDraft(ctx context.Context, id int64) error {
	_, err := a.store.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=NULL WHERE id=$1 AND issue_ref=''`, id)
	return err
}

// SetQQDraftIssue 记下草稿提交成的 issue。
func (a *Service) SetQQDraftIssue(ctx context.Context, id int64, ref string) error {
	_, err := a.store.pool.Exec(ctx, `UPDATE qq_drafts SET issue_ref=$2 WHERE id=$1`, id, ref)
	return err
}

// OpenQQDrafts 列出有效期内还没提交的草稿，最新的在前。
func (a *Service) OpenQQDrafts(ctx context.Context, limit int) ([]QQDraft, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,subject,original,report,created_at FROM qq_drafts WHERE filed_at IS NULL AND created_at>now()-$1::interval ORDER BY created_at DESC LIMIT $2`, QQDraftWindow.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var drafts []QQDraft
	for rows.Next() {
		var d QQDraft
		if err := rows.Scan(&d.ID, &d.Subject, &d.Original, &d.Report, &d.CreatedAt); err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
	}
	return drafts, rows.Err()
}
