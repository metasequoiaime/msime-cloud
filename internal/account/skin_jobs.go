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
	// Expired drafts can hold up to ~12 MB each; dropping them here keeps the table small between the hourly prunes. A cancelled job whose worker went stale is dropped too: it is invisible to GET and DELETE and the worker that would have deleted it is gone, so nothing else ever would before it expires.
	if _, err = tx.Exec(ctx, `DELETE FROM skin_jobs WHERE expires_at<=clock_timestamp() OR (cancelled AND NOT (`+skinJobLive+`))`); err != nil {
		return expires, err
	}
	var all, own int
	if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE owner=$1) FROM skin_jobs WHERE state<>'running' OR `+skinJobLive, owner).Scan(&all, &own); err != nil {
		return expires, err
	}
	if all >= total || own >= perOwner {
		return expires, ErrSkinJobsBusy
	}
	if err = tx.QueryRow(ctx, `INSERT INTO skin_jobs(id,owner,expires_at) VALUES($1,$2,clock_timestamp()+make_interval(secs=>$3)) RETURNING expires_at`, id, owner, ttl.Seconds()).Scan(&expires); err != nil {
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
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// FOR UPDATE orders this against the worker's final write: either the worker recorded its result first and the row is deleted here, or the flag is set first and the worker's write finds it and deletes the row instead.
	var live, valid bool
	err = tx.QueryRow(ctx, `SELECT `+skinJobLive+` FROM skin_jobs WHERE id=$1 AND owner=$2 AND NOT cancelled FOR UPDATE`, id, owner).Scan(&live)
	if err == nil {
		// WHERE 中的时间条件不能在拿到行锁后自动重算；锁等待可能已经跨过任务 TTL。
		if err = tx.QueryRow(ctx, `SELECT expires_at>clock_timestamp(), (`+skinJobLive+`) FROM skin_jobs WHERE id=$1`, id).Scan(&valid, &live); err != nil {
			return err
		}
		if !valid {
			return ErrSkinJobNotFound
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSkinJobNotFound
	}
	if err != nil {
		return err
	}
	if live {
		_, err = tx.Exec(ctx, `UPDATE skin_jobs SET cancelled=true WHERE id=$1`, id)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM skin_jobs WHERE id=$1`, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SkinJobHeartbeat refreshes a running job's heartbeat and reports whether its worker should keep going. It returns false once the job was cancelled by a DELETE on any replica, has expired, or already went stale (other replicas report it failed, so a late result must not revive it).
func (a *Service) SkinJobHeartbeat(ctx context.Context, id string) (bool, error) {
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var live bool
	err = tx.QueryRow(ctx, `SELECT NOT cancelled FROM skin_jobs WHERE id=$1 FOR UPDATE`, id).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if err = tx.QueryRow(ctx, `SELECT NOT cancelled AND expires_at>clock_timestamp() AND (`+skinJobLive+`) FROM skin_jobs WHERE id=$1`, id).Scan(&live); err != nil {
		return false, err
	}
	if live {
		if _, err = tx.Exec(ctx, `UPDATE skin_jobs SET heartbeat_at=clock_timestamp() WHERE id=$1`, id); err != nil {
			return false, err
		}
	}
	return live, tx.Commit(ctx)
}

// FinishSkinJob 记录执行者的成功插画或失败原因；任务已取消（此处删除记录）、心跳失效或过期时不写入结果并返回 false。
func (a *Service) FinishSkinJob(ctx context.Context, id, state, reason string, artwork []byte) (bool, error) {
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var cancelled bool
	err = tx.QueryRow(ctx, `SELECT cancelled FROM skin_jobs WHERE id=$1 FOR UPDATE`, id).Scan(&cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	var live bool
	if err = tx.QueryRow(ctx, `SELECT NOT cancelled AND expires_at>clock_timestamp() AND (`+skinJobLive+`) FROM skin_jobs WHERE id=$1`, id).Scan(&live); err != nil {
		return false, err
	}
	if live {
		if _, err = tx.Exec(ctx, `UPDATE skin_jobs SET state=$2,reason=$3,artwork=$4,heartbeat_at=clock_timestamp() WHERE id=$1`, id, state, reason, artwork); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	if cancelled {
		if _, err = tx.Exec(ctx, `DELETE FROM skin_jobs WHERE id=$1`, id); err != nil {
			return false, err
		}
	}
	return false, tx.Commit(ctx)
}

// DropSkinJob deletes a job that was recorded but never started, for a replica that began shutting down between the insert and starting its worker.
func (a *Service) DropSkinJob(ctx context.Context, id string) error {
	_, err := a.store.pool.Exec(ctx, `DELETE FROM skin_jobs WHERE id=$1`, id)
	return err
}
