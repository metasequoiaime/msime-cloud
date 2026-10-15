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

// CreateSkinJob records a new running job for owner unless owner already holds perOwner unexpired jobs or the deployment holds total. Jobs a DELETE cancelled still count until their worker has stopped, as the in-process implementation counted them until the upstream call returned. A running job whose worker stopped heartbeating does not count: its replica died (or lost the database for longer than SkinJobStaleAfter, after which its worker is told to stop), and in the in-process implementation a dead replica freed its slots with it. Without this a crashed replica's orphaned jobs, which no client may ever delete, would hold the caps until they expire. It returns the job's expiry.
func (a *Service) CreateSkinJob(ctx context.Context, id, owner string, perOwner, total int, ttl time.Duration) (time.Time, error) {
	var expires time.Time
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return expires, err
	}
	defer tx.Rollback(ctx)
	// The lock is held only for a few statements, but a replica whose node vanishes inside them would otherwise block every replica's job creation until the kernel reaps the dead connection.
	if _, err = tx.Exec(ctx, serverKeepalives); err != nil {
		return expires, err
	}
	// 用户任务在共享计数和写入前锁定账号行，保证管理员封禁与任务创建按提交顺序线性化；配置客户端的 owner 不对应 auth_users，沿用原有路径。
	if uid, ok := strings.CutPrefix(owner, "user:"); ok {
		var banned bool
		if err = tx.QueryRow(ctx, "SELECT banned_at IS NOT NULL FROM auth_users WHERE id=$1 FOR UPDATE", uid).Scan(&banned); errors.Is(err, pgx.ErrNoRows) {
			return expires, ErrInvalid
		} else if err != nil {
			return expires, err
		}
		if banned {
			return expires, ErrBanned
		}
	}
	if _, err = tx.Exec(ctx, skinJobsLock); err != nil {
		return expires, err
	}
	// 先清理过期任务和失去工作进程的已取消任务，再基于清理后的快照判断容量并插入。
	// 两条语句批量发送，保留执行顺序；达到容量上限时回滚，清理也不会单独提交。
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM skin_jobs WHERE expires_at<=clock_timestamp() OR (cancelled AND NOT (` + skinJobLive + `))`)
	batch.Queue(`INSERT INTO skin_jobs(id,owner,expires_at)
SELECT $1,$2,clock_timestamp()+make_interval(secs=>$3)
FROM (SELECT count(*) AS total_count,count(*) FILTER (WHERE owner=$2) AS owner_count
 FROM skin_jobs WHERE state<>'running' OR `+skinJobLive+`) counts
WHERE owner_count<$4 AND total_count<$5
RETURNING expires_at`, id, owner, ttl.Seconds(), perOwner, total).QueryRow(func(row pgx.Row) error {
		return row.Scan(&expires)
	})
	if err = tx.SendBatch(ctx, batch).Close(); errors.Is(err, pgx.ErrNoRows) {
		return expires, ErrSkinJobsBusy
	} else if err != nil {
		return expires, err
	}
	return expires, tx.Commit(ctx)
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

// DropSkinJob deletes a job that was recorded but never started, for a replica that began shutting down between the insert and starting its worker.
func (a *Service) DropSkinJob(ctx context.Context, id string) error {
	_, err := a.store.pool.Exec(ctx, `DELETE FROM skin_jobs WHERE id=$1`, id)
	return err
}
