package account

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSkinJobsBusy is returned by CreateSkinJob when the owner or the whole deployment already holds as many artwork jobs as allowed.
var ErrSkinJobsBusy = errors.New("skin_jobs_busy")

// ErrSkinJobNotFound is returned for a job that does not exist, has expired, was deleted or belongs to someone else; the caller cannot tell these apart, so another principal cannot probe for job ids.
var ErrSkinJobNotFound = errors.New("skin_job_not_found")

// SkinJobStaleAfter is how long a running job may go without a heartbeat from the replica executing it before every replica reports it as failed. The executing worker refreshes it every few seconds, so only a replica that died (SIGKILL, OOM, lost node) or lost the database for this long misses it; the worker itself stops once its heartbeat is refused, so a stale job never turns into a success later.
const SkinJobStaleAfter = 30 * time.Second

// skinJobsLock serialises the cap check and insert of every replica: the counts it reads must not change before the insert commits. Job creation is rare (a handful per minute), so one deployment-wide lock costs nothing.
const skinJobsLock = `SELECT pg_advisory_xact_lock(hashtextextended('skin_jobs',0))`

// skinJobLive is true for a running job whose worker is still heartbeating.
var skinJobLive = `state='running' AND heartbeat_at>=clock_timestamp()-make_interval(secs=>` + strconv.Itoa(int(SkinJobStaleAfter/time.Second)) + `)`

// SkinJob is what GET /v1/skins/jobs/{job} reports. A running job whose worker stopped heartbeating is reported as failed with reason "cancelled", the same terminal state a job interrupted by a graceful shutdown ends in.
type SkinJob struct {
	State, Reason string
	// Artwork is the synchronous endpoint's JSON body; nil unless State is "succeeded".
	Artwork []byte
}

// SkinJobReservation 保存任务到期时间及仅用于撤回未启动任务的内部凭据。
// 凭据绑定任务身份和原额度窗口，防止重复退款或影响后续任务。
type SkinJobReservation struct {
	ExpiresAt      time.Time
	id, owner      string
	quotaExpiresAt time.Time
}

// CreateSkinJob 在同一事务中检查主体与全局容量、记录任务并扣除每日额度；任何一步失败都会回滚。
// 返回任务预留凭据；每日额度用尽时返回剩余等待时长与 `ErrLimited`。
// 已取消但工作进程尚未停止的任务仍占容量；失去心跳的任务不占容量。
func (a *Service) CreateSkinJob(ctx context.Context, id, owner string, perOwner, total, perDay int, ttl time.Duration) (SkinJobReservation, time.Duration, error) {
	reservation := SkinJobReservation{id: id, owner: owner}
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return reservation, 0, err
	}
	defer tx.Rollback(ctx)
	// 保活设置与首个锁请求批量发送，仍在取锁前启用，避免持锁副本失联后长时间阻塞其他副本。
	setup := &pgx.Batch{}
	for _, statement := range strings.Split(serverKeepalives, ";") {
		setup.Queue(statement)
	}
	// 用户任务在共享计数和写入前锁定账号行，保证管理员封禁与任务创建按提交顺序线性化；配置客户端的 owner 不对应 auth_users，沿用原有路径。
	uid, userOwner := strings.CutPrefix(owner, "user:")
	if userOwner {
		setup.Queue("SELECT banned_at IS NOT NULL FROM auth_users WHERE id=$1 FOR UPDATE", uid).QueryRow(func(row pgx.Row) error {
			var banned bool
			if err := row.Scan(&banned); errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalid
			} else if err != nil {
				return err
			}
			if banned {
				return ErrBanned
			}
			return nil
		})
	} else {
		setup.Queue(skinJobsLock)
	}
	if err = tx.SendBatch(ctx, setup).Close(); err != nil {
		return reservation, 0, err
	}
	if userOwner {
		// 先确认账号有效再等待全局锁；后续容量读取使用取锁后的独立语句快照。
		if _, err = tx.Exec(ctx, skinJobsLock); err != nil {
			return reservation, 0, err
		}
	}
	// 先清理过期任务和失去工作进程的已取消任务，再基于清理后的快照判断容量并插入。
	// 清理、插入和每日额度批量发送，保留执行顺序；达到任一上限时回滚，清理和扣额也不会单独提交。
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM skin_jobs WHERE expires_at<=clock_timestamp() OR (cancelled AND NOT (` + skinJobLive + `))`)
	// 初始心跳必须取锁后的实际时间；默认 now() 固定为事务开始时间，长锁等待会让新任务立即失效。
	batch.Queue(`INSERT INTO skin_jobs(id,owner,expires_at,heartbeat_at)
SELECT $1,$2,clock_timestamp()+make_interval(secs=>$3),clock_timestamp()
FROM (SELECT count(*) AS total_count,count(*) FILTER (WHERE owner=$2) AS owner_count
 FROM skin_jobs WHERE state<>'running' OR `+skinJobLive+`) counts
WHERE owner_count<$4 AND total_count<$5
RETURNING expires_at`, id, owner, ttl.Seconds(), perOwner, total).QueryRow(func(row pgx.Row) error {
		return row.Scan(&reservation.ExpiresAt)
	})
	var wait time.Duration
	batch.Queue(rateUntilSQL, "skin-art-day:"+hash(owner), (24 * time.Hour).String(), perDay).QueryRow(func(row pgx.Row) error {
		var used int
		var now time.Time
		if err := row.Scan(&used, &reservation.quotaExpiresAt, &now); err != nil {
			return err
		}
		if used > perDay {
			wait = max(reservation.quotaExpiresAt.Sub(now), time.Second)
			return ErrLimited
		}
		return nil
	})
	if err = tx.SendBatch(ctx, batch).Close(); errors.Is(err, pgx.ErrNoRows) {
		return reservation, 0, ErrSkinJobsBusy
	} else if err != nil {
		return reservation, wait, err
	}
	return reservation, 0, tx.Commit(ctx)
}

// GetSkinJob returns owner's unexpired, undeleted job.
func (a *Service) GetSkinJob(ctx context.Context, id, owner string) (SkinJob, error) {
	var job SkinJob
	err := a.store.pool.QueryRow(ctx, `SELECT CASE WHEN state='running' AND NOT (`+skinJobLive+`) THEN 'failed' ELSE state END,
 CASE WHEN state='running' AND NOT (`+skinJobLive+`) THEN 'cancelled' ELSE reason END,artwork
 FROM skin_jobs WHERE id=$1 AND owner=$2 AND NOT cancelled AND expires_at>clock_timestamp()`, id, owner).Scan(&job.State, &job.Reason, &job.Artwork)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, ErrSkinJobNotFound
	}
	return job, err
}

// DeleteSkinJob removes owner's job. A finished job (or one whose worker died) is deleted at once; a running one is flagged cancelled and the worker executing it, on whichever replica, stops the upstream call and deletes the row. Either way the job is gone for GET immediately.
func (a *Service) DeleteSkinJob(ctx context.Context, id, owner string) error {
	// 在一条语句里锁定并重新计算任务状态，再按心跳状态取消或删除。锁等待可能跨过 TTL，所以不能复用锁前的时间判断。
	var outcome string
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
	 SELECT id,state,heartbeat_at,expires_at
	 FROM skin_jobs WHERE id=$1 AND owner=$2 AND NOT cancelled FOR UPDATE
), classified AS MATERIALIZED (
	 SELECT id,expires_at>clock_timestamp() AS valid,
		(state='running' AND heartbeat_at>=clock_timestamp()-make_interval(secs=>`+strconv.Itoa(int(SkinJobStaleAfter/time.Second))+`)) AS live
	 FROM locked
), removed AS (
	 DELETE FROM skin_jobs s USING classified l
 WHERE s.id=l.id AND l.valid AND NOT l.live
 RETURNING 'deleted'::text AS outcome
), cancelled AS (
	 UPDATE skin_jobs s SET cancelled=true FROM classified l
 WHERE s.id=l.id AND l.valid AND l.live
 RETURNING 'cancelled'::text AS outcome
)
SELECT COALESCE((SELECT outcome FROM removed),(SELECT outcome FROM cancelled),CASE WHEN EXISTS(SELECT 1 FROM locked) THEN 'expired' ELSE 'missing' END)`, id, owner).Scan(&outcome)
	if err != nil {
		return err
	}
	if outcome == "expired" || outcome == "missing" {
		return ErrSkinJobNotFound
	}
	return nil
}

// SkinJobHeartbeat refreshes a running job's heartbeat and reports whether its worker should keep going. It returns false once the job was cancelled by a DELETE on any replica, has expired, or already went stale (other replicas report it failed, so a late result must not revive it).
func (a *Service) SkinJobHeartbeat(ctx context.Context, id string) (bool, error) {
	var live bool
	// 先锁定任务行，再在独立的物化 CTE 中计算锁后的有效期和心跳状态；一次语句完成读取与心跳更新。
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,cancelled,state,heartbeat_at,expires_at
 FROM skin_jobs WHERE id=$1 FOR UPDATE
), classified AS MATERIALIZED (
 SELECT id,NOT cancelled AND expires_at>clock_timestamp() AND
	(state='running' AND heartbeat_at>=clock_timestamp()-make_interval(secs=>`+strconv.Itoa(int(SkinJobStaleAfter/time.Second))+`)) AS live
 FROM locked
), updated AS (
 UPDATE skin_jobs s SET heartbeat_at=clock_timestamp()
 FROM classified c WHERE s.id=c.id AND c.live
 RETURNING 1
)
SELECT COALESCE((SELECT live FROM classified),false)`, id).Scan(&live)
	if err != nil {
		return false, err
	}
	return live, nil
}

// FinishSkinJob 记录执行者的成功插画或失败原因；任务已取消（此处删除记录）、心跳失效或过期时不写入结果并返回 false。
func (a *Service) FinishSkinJob(ctx context.Context, id, state, reason string, artwork []byte) (bool, error) {
	// 先锁定任务行，再在独立的物化 CTE 中计算锁后的有效期和心跳状态；一次语句完成读取、结果更新和取消任务删除。
	var recorded bool
	err := a.store.pool.QueryRow(ctx, `WITH locked AS MATERIALIZED (
 SELECT id,cancelled,state,heartbeat_at,expires_at
 FROM skin_jobs WHERE id=$1 FOR UPDATE
), classified AS MATERIALIZED (
 SELECT id,cancelled,expires_at>clock_timestamp() AS valid,
  (NOT cancelled AND state='running' AND heartbeat_at>=clock_timestamp()-make_interval(secs=>`+strconv.Itoa(int(SkinJobStaleAfter/time.Second))+`)) AS live
 FROM locked
), updated AS (
 UPDATE skin_jobs s
 SET state=$2,reason=$3,artwork=$4,heartbeat_at=clock_timestamp()
 FROM classified c
 WHERE s.id=c.id AND c.valid AND c.live
 RETURNING 1
), removed AS (
 DELETE FROM skin_jobs s
 USING classified c
 WHERE s.id=c.id AND c.cancelled
 RETURNING 1
)
SELECT EXISTS(SELECT 1 FROM updated)`, id, state, reason, artwork).Scan(&recorded)
	if err != nil {
		return false, err
	}
	return recorded, nil
}

// DropSkinJob 原子撤回副本关闭时已登记但未启动的任务，并退还原窗口的一次额度。
// 只有凭据对应的任务实际被删除才退款；重试不重复退款，旧凭据也不影响复用 ID 的新任务。
func (a *Service) DropSkinJob(ctx context.Context, reservation SkinJobReservation) error {
	_, err := a.store.pool.Exec(ctx, `WITH removed AS (
 DELETE FROM skin_jobs WHERE id=$1 AND owner=$2 AND expires_at=$3
 RETURNING 1
)
UPDATE auth_rates SET count=greatest(count-1,0)
WHERE key=$4 AND expires_at=$5 AND EXISTS(SELECT 1 FROM removed)`,
		reservation.id, reservation.owner, reservation.ExpiresAt,
		"skin-art-day:"+hash(reservation.owner), reservation.quotaExpiresAt)
	return err
}
