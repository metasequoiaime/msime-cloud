package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// QQDraftWindow 是草稿的有效期：更早的不再列出、不能提交，并由 Store.Prune 删除。
const QQDraftWindow = 30 * 24 * time.Hour

// QQDraftClaimTimeout 是认领的有效期：认领超过这么久还没记下 issue，就当作提交被打断了（例如副本被 SIGKILL，连放回认领都没来得及），草稿可以重新认领、补充，也重新出现在列表里。它要比 QQ 机器人一条消息的处理时限（4 分钟）长，正常还在提交的草稿不会被另一个副本抢走。
const QQDraftClaimTimeout = 5 * time.Minute

// QQDraftIdempotencyWindow 是同一条草稿写请求的幂等窗口。请求结果不确定时，短时间内重试应返回已经写入的草稿，而不是再建一份。
const QQDraftIdempotencyWindow = 10 * time.Minute

// qqDraftUnclaimed 是「可以认领」的条件：没被认领，或者认领已经超时而且没记下 issue。
var qqDraftUnclaimed = `(filed_at IS NULL OR (issue_ref='' AND filed_at<clock_timestamp()-'` + QQDraftClaimTimeout.String() + `'::interval))`

// ErrQQDraftNotFound 表示草稿不存在或已经过期。
var ErrQQDraftNotFound = errors.New("qq draft not found")

// ErrQQDraftChanged 表示整理期间草稿内容已变化，本次补充没有保存。
var ErrQQDraftChanged = errors.New("qq draft changed")

// QQDraftIgnored 是管理员「忽略 D编号」后草稿的 issue_ref：被忽略的草稿和提交过的一样 filed_at 非空，不再列出、不能补充和提交，只是没有对应的 issue。
const QQDraftIgnored = "ignored"

// QQDraft 是一份 issue 草稿。Subject 是群和成员标识的摘要。UpdatedAt 是新建或最后一次补充的时间（QQDraft 读出，OpenQQDrafts 不读）。Filed 表示已经被认领提交（或者被忽略），FiledAt 是认领的时间（没认领时为零值）；IssueRef 为空时还在提交，或者提交被打断了，见 QQDraftClaimStale；为 QQDraftIgnored 时是被管理员忽略了。
type QQDraft struct {
	ID        int64
	Subject   string
	Original  string
	Report    string
	CreatedAt time.Time
	UpdatedAt time.Time
	Filed     bool
	FiledAt   time.Time
	IssueRef  string
}

// CreateQQDraft 新建一份草稿，subject 由调用方拼成，这里只存摘要。
func (a *Service) CreateQQDraft(ctx context.Context, subject, original, report string) (int64, error) {
	var id int64
	err := a.store.pool.QueryRow(ctx, `INSERT INTO qq_drafts(subject,original,report) VALUES($1,$2,$3) RETURNING id`, QQDraftSubject(subject), original, report).Scan(&id)
	return id, err
}

// CreateQQDraftOnce 在短时间内对相同内容只新建一份草稿，返回已有或新建的编号。用事务级 advisory lock 串行化多个副本，处理提交成功但响应丢失后的重试。
func (a *Service) CreateQQDraftOnce(ctx context.Context, subject, original, report string) (int64, error) {
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	subject = QQDraftSubject(subject)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || chr(31) || $2 || chr(31) || $3))`, subject, original, report); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `SELECT id FROM qq_drafts WHERE subject=$1 AND original=$2 AND report=$3 AND created_at>now()-$4::interval ORDER BY id LIMIT 1`, subject, original, report, QQDraftIdempotencyWindow.String()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO qq_drafts(subject,original,report) VALUES($1,$2,$3) RETURNING id`, subject, original, report).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

// QQDraft 读一份有效期内的草稿。
func (a *Service) QQDraft(ctx context.Context, id int64) (QQDraft, error) {
	var d QQDraft
	var filedAt *time.Time
	err := a.store.pool.QueryRow(ctx, `SELECT id,subject,original,report,created_at,updated_at,filed_at,issue_ref FROM qq_drafts WHERE id=$1 AND created_at>now()-$2::interval`, id, QQDraftWindow.String()).Scan(&d.ID, &d.Subject, &d.Original, &d.Report, &d.CreatedAt, &d.UpdatedAt, &filedAt, &d.IssueRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrQQDraftNotFound
	}
	if filedAt != nil {
		d.Filed, d.FiledAt = true, *filedAt
	}
	return d, err
}

// QQDraftSubject 是草稿里存的 subject 摘要。
func QQDraftSubject(subject string) string { return hash(subject) }

// QQDraftOwnedBy 判断草稿是不是 subject 的。
func QQDraftOwnedBy(d QQDraft, subject string) bool { return d.Subject == QQDraftSubject(subject) }

// QQDraftClaimStale 判断草稿的认领是不是已经超时：认领了、没记下 issue，而且认领超过 QQDraftClaimTimeout。这样的草稿和没认领的一样可以再提交。
func QQDraftClaimStale(d QQDraft, now time.Time) bool {
	return d.Filed && d.IssueRef == "" && now.Sub(d.FiledAt) > QQDraftClaimTimeout
}

// UpdateQQDraft 更新还没提交的草稿的原话和整理结果，返回是否更新了。认领超时的草稿也算没提交；内容必须仍与整理前的快照相同，否则返回 ErrQQDraftChanged。
func (a *Service) UpdateQQDraft(ctx context.Context, previous QQDraft, original, report string) (bool, error) {
	var available, updated bool
	// 先锁定并读取当前内容，再检查锁后的有效期和认领状态；等待其他补充提交后仍能识别冲突，模型整理期间不持锁。
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
SELECT id,original,report,created_at,filed_at,issue_ref FROM qq_drafts WHERE id=$1 FOR UPDATE
), current AS MATERIALIZED (
SELECT id,original,report FROM locked WHERE `+qqDraftUnclaimed+` AND created_at>clock_timestamp()-$4::interval
), updated AS (
UPDATE qq_drafts SET original=$2,report=$3,updated_at=clock_timestamp()
WHERE id IN (SELECT id FROM current WHERE original=$5 AND report=$6) RETURNING id
)
SELECT EXISTS(SELECT 1 FROM current),EXISTS(SELECT 1 FROM updated)`, previous.ID, original, report, QQDraftWindow.String(), previous.Original, previous.Report).Scan(&available, &updated)
	if err == nil && available && !updated {
		return false, ErrQQDraftChanged
	}
	return updated, err
}

// ClaimQQDraft 认领一份还没提交的草稿准备提交，返回是否认领到：两个副本同时收到「提交」时只有一个能认领，不会提两遍。认领超过 QQDraftClaimTimeout 还没记下 issue 的草稿可以重新认领，提交中途副本被杀掉、来不及放回认领时，草稿不会永远卡在「提交中」。
func (a *Service) ClaimQQDraft(ctx context.Context, id int64) (bool, error) {
	_, ok, err := a.ClaimQQDraftLease(ctx, id)
	return ok, err
}

// ClaimQQDraftLease 认领草稿并返回本次认领的时间戳。完成或放回时必须带回这个时间戳，避免旧副本在认领超时后覆盖新副本的结果。
func (a *Service) ClaimQQDraftLease(ctx context.Context, id int64) (time.Time, bool, error) {
	draft, ok, err := a.ClaimQQDraftSnapshot(ctx, id)
	return draft.FiledAt, ok, err
}

// ClaimQQDraftSnapshot 在认领的同一条语句中返回当前内容，避免提交读取草稿之后、认领之前已被补充的旧报告。
func (a *Service) ClaimQQDraftSnapshot(ctx context.Context, id int64) (QQDraft, bool, error) {
	var draft QQDraft
	// 不能在等锁前判断草稿和租约的有效期，过期边界必须以锁后的时钟为准。
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
SELECT id,created_at,filed_at,issue_ref FROM qq_drafts WHERE id=$1 FOR UPDATE
)
UPDATE qq_drafts SET filed_at=clock_timestamp()
WHERE id IN (SELECT id FROM locked WHERE `+qqDraftUnclaimed+` AND created_at>clock_timestamp()-$2::interval)
RETURNING id,subject,original,report,created_at,updated_at,filed_at,issue_ref`, id, QQDraftWindow.String()).Scan(&draft.ID, &draft.Subject, &draft.Original, &draft.Report, &draft.CreatedAt, &draft.UpdatedAt, &draft.FiledAt, &draft.IssueRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return QQDraft{}, false, nil
	}
	draft.Filed = err == nil
	return draft, err == nil, err
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

// SetQQDraftIssueLease 只为仍属于本次认领的草稿记下 issue，返回是否成功写入。
func (a *Service) SetQQDraftIssueLease(ctx context.Context, id int64, claimedAt time.Time, ref string) (bool, error) {
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_drafts SET issue_ref=$2 WHERE id=$1 AND issue_ref='' AND filed_at=$3`, id, ref, claimedAt)
	return tag.RowsAffected() == 1, err
}

// ReleaseQQDraftLease 只释放仍属于本次认领且尚未记下 issue 的草稿。
func (a *Service) ReleaseQQDraftLease(ctx context.Context, id int64, claimedAt time.Time) (bool, error) {
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_drafts SET filed_at=NULL WHERE id=$1 AND issue_ref='' AND filed_at=$2`, id, claimedAt)
	return tag.RowsAffected() == 1, err
}

// DismissQQDraft 忽略一份还没提交的草稿（包括认领超时的），返回是否忽略了：已经在提交、提交过或者忽略过的不改。忽略后 filed_at 记成现在、issue_ref 记成 QQDraftIgnored，不再列出，也不能再补充或提交。
func (a *Service) DismissQQDraft(ctx context.Context, id int64) (bool, error) {
	// 先锁定草稿，再判断有效期和认领状态，与补充、认领的时钟规则一致。
	tag, err := a.store.pool.Exec(ctx, `WITH locked AS MATERIALIZED (
SELECT id,created_at,filed_at,issue_ref FROM qq_drafts WHERE id=$1 FOR UPDATE
)
UPDATE qq_drafts SET filed_at=clock_timestamp(),issue_ref=$3
WHERE id IN (SELECT id FROM locked WHERE `+qqDraftUnclaimed+` AND created_at>clock_timestamp()-$2::interval)`, id, QQDraftWindow.String(), QQDraftIgnored)
	return tag.RowsAffected() == 1, err
}

// OpenQQDrafts 列出有效期内还没提交的草稿（包括认领超时的），最早的在前，另外返回这样的草稿一共有多少份。原来最新的在前，积压超过 limit 份时最早的那些永远排不到，30 天后被 Store.Prune 悄悄删掉；最早的在前，快过期的先被看到。
func (a *Service) OpenQQDrafts(ctx context.Context, limit int) ([]QQDraft, int, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,subject,original,report,created_at,count(*) OVER() FROM qq_drafts WHERE `+qqDraftUnclaimed+` AND created_at>now()-$1::interval ORDER BY created_at ASC, id ASC LIMIT $2`, QQDraftWindow.String(), limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var drafts []QQDraft
	total := 0
	for rows.Next() {
		var d QQDraft
		if err := rows.Scan(&d.ID, &d.Subject, &d.Original, &d.Report, &d.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		drafts = append(drafts, d)
	}
	return drafts, total, rows.Err()
}
