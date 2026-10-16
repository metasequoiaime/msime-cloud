package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// QQFAQCandidateWindow 是文档候选的有效期：更早的不再列出，并由 Store.Prune 删除。
const QQFAQCandidateWindow = 30 * 24 * time.Hour

// QQFAQCandidateClaimTimeout 是「写进FAQ」认领的有效期：认领超过这么久还在 writing，就当作写入被打断了（例如副本被 SIGKILL，连放回认领都没来得及），候选可以重新认领、忽略，也重新出现在列表里。和 QQDraftClaimTimeout 一样要比 QQ 机器人一条消息的处理时限（4 分钟）长，正常还在开 PR 的候选不会被另一个副本抢走。
const QQFAQCandidateClaimTimeout = 5 * time.Minute

// qqFAQCandidateClaimable 是「可以认领」的条件：还没处理，或者正在写入但认领已经超时。
var qqFAQCandidateClaimable = `(state='open' OR (state='writing' AND claimed_at<clock_timestamp()-'` + QQFAQCandidateClaimTimeout.String() + `'::interval))`

// ErrQQFAQCandidateNotFound 表示候选不存在、已经处理或者过期。
var ErrQQFAQCandidateNotFound = errors.New("qq faq candidate not found")

// QQFAQCandidate 是一条文档候选。Writing 表示有人认领了正在写进 FAQ，ClaimedAt 是认领的时间（没认领时为零值），认领超时见 QQFAQCandidateClaimStale。
type QQFAQCandidate struct {
	ID        int64
	Question  string
	Answer    string
	CreatedAt time.Time
	Writing   bool
	ClaimedAt time.Time
}

// QQFAQCandidateClaimStale 判断候选的认领是不是已经超时：正在写入，而且认领超过 QQFAQCandidateClaimTimeout。这样的候选和没认领的一样可以再处理。
func QQFAQCandidateClaimStale(c QQFAQCandidate, now time.Time) bool {
	return c.Writing && now.Sub(c.ClaimedAt) > QQFAQCandidateClaimTimeout
}

// AddQQFAQCandidate 记下一条文档候选。
func (a *Service) AddQQFAQCandidate(ctx context.Context, question, answer string) (int64, error) {
	var id int64
	err := a.store.pool.QueryRow(ctx, `INSERT INTO qq_faq_candidates(question,answer) VALUES($1,$2) RETURNING id`, question, answer).Scan(&id)
	return id, err
}

// QQFAQCandidate 读一条有效期内、还没处理完的候选，包括正在写进 FAQ 的（Writing）。
func (a *Service) QQFAQCandidate(ctx context.Context, id int64) (QQFAQCandidate, error) {
	var c QQFAQCandidate
	var claimedAt *time.Time
	err := a.store.pool.QueryRow(ctx, `SELECT id,question,answer,created_at,state='writing',claimed_at FROM qq_faq_candidates WHERE id=$1 AND state IN ('open','writing') AND created_at>now()-$2::interval`, id, QQFAQCandidateWindow.String()).Scan(&c.ID, &c.Question, &c.Answer, &c.CreatedAt, &c.Writing, &claimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrQQFAQCandidateNotFound
	}
	if claimedAt != nil {
		c.ClaimedAt = *claimedAt
	}
	return c, err
}

// OpenQQFAQCandidates 列出有效期内还没处理的候选（包括认领超时的），最早的在前，另外返回这样的候选一共有多少条：和 OpenQQDrafts 一样，积压超过 limit 条时最早的那些原来排不到就过期了。
func (a *Service) OpenQQFAQCandidates(ctx context.Context, limit int) ([]QQFAQCandidate, int, error) {
	rows, err := a.store.pool.Query(ctx, `SELECT id,question,answer,created_at,count(*) OVER() FROM qq_faq_candidates WHERE `+qqFAQCandidateClaimable+` AND created_at>now()-$1::interval ORDER BY created_at ASC, id ASC LIMIT $2`, QQFAQCandidateWindow.String(), limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []QQFAQCandidate
	total := 0
	for rows.Next() {
		var c QQFAQCandidate
		if err := rows.Scan(&c.ID, &c.Question, &c.Answer, &c.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ClaimQQFAQCandidate 认领一条还没处理的候选准备写进 FAQ（state 改成 writing），返回是否认领到：两个管理员同时发「写进FAQ F3」时只有一个能认领，不会开两个 PR。认领超过 QQFAQCandidateClaimTimeout 还在 writing 的可以重新认领。
func (a *Service) ClaimQQFAQCandidate(ctx context.Context, id int64) (bool, error) {
	_, ok, err := a.ClaimQQFAQCandidateLease(ctx, id)
	return ok, err
}

// ClaimQQFAQCandidateLease 认领候选并返回本次认领的时间戳。关闭或放回时必须带回这个时间戳，避免旧副本覆盖新副本的处理结果。
func (a *Service) ClaimQQFAQCandidateLease(ctx context.Context, id int64) (time.Time, bool, error) {
	var claimedAt time.Time
	// 先取得行锁，再判断候选有效期和认领租约，避免等待锁时跨过边界。
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
SELECT id,state,claimed_at,created_at FROM qq_faq_candidates WHERE id=$1 FOR UPDATE
)
UPDATE qq_faq_candidates SET state='writing',claimed_at=clock_timestamp()
WHERE id IN (SELECT id FROM locked WHERE `+qqFAQCandidateClaimable+` AND created_at>clock_timestamp()-$2::interval)
RETURNING claimed_at`, id, QQFAQCandidateWindow.String()).Scan(&claimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return claimedAt, err == nil, err
}

// ReleaseQQFAQCandidate 在没开成 PR 时放回认领（改回 open），之后可以再写进 FAQ 或忽略。
func (a *Service) ReleaseQQFAQCandidate(ctx context.Context, id int64) error {
	_, err := a.store.pool.Exec(ctx, `UPDATE qq_faq_candidates SET state='open',claimed_at=NULL WHERE id=$1 AND state='writing'`, id)
	return err
}

// ReleaseQQFAQCandidateLease 只释放仍属于本次认领的候选。
func (a *Service) ReleaseQQFAQCandidateLease(ctx context.Context, id int64, claimedAt time.Time) (bool, error) {
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_faq_candidates SET state='open',claimed_at=NULL WHERE id=$1 AND state='writing' AND claimed_at=$2`, id, claimedAt)
	return tag.RowsAffected() == 1, err
}

// CloseQQFAQCandidate 把候选标成 done（写进了 FAQ，ref 是 PR）或 ignored，返回是否改了（已经处理过的不再改）。done 也可以从 writing 改（认领后开成了 PR）；ignored 只改还没处理、或者认领已经超时的，正在写入的不能忽略。
func (a *Service) CloseQQFAQCandidate(ctx context.Context, id int64, state, ref string) (bool, error) {
	if state != "done" && state != "ignored" {
		return false, errors.New("invalid candidate state")
	}
	// 忽略操作也按锁后的有效期判断；已开成 PR 的完成记录仍可收尾。
	tag, err := a.store.pool.Exec(ctx, `WITH locked AS MATERIALIZED (
SELECT id,state,claimed_at,created_at FROM qq_faq_candidates WHERE id=$1 FOR UPDATE
)
UPDATE qq_faq_candidates SET state=$2,ref=$3,claimed_at=NULL
WHERE id IN (SELECT id FROM locked WHERE (`+qqFAQCandidateClaimable+` OR ($2='done' AND state='writing'))
AND ($2='done' OR created_at>clock_timestamp()-$4::interval))`, id, state, ref, QQFAQCandidateWindow.String())
	return tag.RowsAffected() == 1, err
}

// CloseQQFAQCandidateLease 只关闭仍属于本次认领的候选；过期认领重新被其他副本接手后，旧副本的写入会被忽略。
func (a *Service) CloseQQFAQCandidateLease(ctx context.Context, id int64, claimedAt time.Time, state, ref string) (bool, error) {
	if state != "done" && state != "ignored" {
		return false, errors.New("invalid candidate state")
	}
	tag, err := a.store.pool.Exec(ctx, `UPDATE qq_faq_candidates SET state=$2,ref=$3,claimed_at=NULL WHERE id=$1 AND state='writing' AND claimed_at=$4`, id, state, ref, claimedAt)
	return tag.RowsAffected() == 1, err
}
