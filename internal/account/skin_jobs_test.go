package account

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func skinJobID(c byte) string { return strings.Repeat(string(c), 48) }

// The caps hold across callers sharing the table, a cancelled job keeps its slot until its worker has finished, and a stale job is failed for good.
func TestSkinJobStoreCapsCancelAndStaleness(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []byte{'a', 'b'} {
		if _, _, err := a.CreateSkinJob(ctx, skinJobID(c), "owner-1", 2, 3, 10, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('c'), "owner-1", 2, 3, 10, time.Minute); !errors.Is(err, ErrSkinJobsBusy) {
		t.Fatal("per-owner cap", err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('c'), "owner-2", 2, 3, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('d'), "owner-2", 2, 3, 10, time.Minute); !errors.Is(err, ErrSkinJobsBusy) {
		t.Fatal("global cap", err)
	}
	if err := a.DeleteSkinJob(ctx, skinJobID('a'), "owner-2"); !errors.Is(err, ErrSkinJobNotFound) {
		t.Fatal("cross-owner delete", err)
	}
	if err := a.DeleteSkinJob(ctx, skinJobID('a'), "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetSkinJob(ctx, skinJobID('a'), "owner-1"); !errors.Is(err, ErrSkinJobNotFound) {
		t.Fatal("cancelled job visible", err)
	}
	if alive, err := a.SkinJobHeartbeat(ctx, skinJobID('a')); err != nil || alive {
		t.Fatal("cancelled worker told to continue", alive, err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('d'), "owner-2", 2, 3, 10, time.Minute); !errors.Is(err, ErrSkinJobsBusy) {
		t.Fatal("cancelled job released its slot before its worker stopped", err)
	}
	if recorded, err := a.FinishSkinJob(ctx, skinJobID('a'), "failed", "cancelled", nil); err != nil || recorded {
		t.Fatal(recorded, err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('d'), "owner-2", 2, 3, 10, time.Minute); err != nil {
		t.Fatal("slot not released", err)
	}

	if _, err := db.pool.Exec(ctx, `UPDATE skin_jobs SET heartbeat_at=now()-interval '1 minute' WHERE id=$1`, skinJobID('b')); err != nil {
		t.Fatal(err)
	}
	if job, err := a.GetSkinJob(ctx, skinJobID('b'), "owner-1"); err != nil || job.State != "failed" || job.Reason != "cancelled" || job.Artwork != nil {
		t.Fatal("stale job", job, err)
	}
	if alive, err := a.SkinJobHeartbeat(ctx, skinJobID('b')); err != nil || alive {
		t.Fatal("stale worker told to continue", alive, err)
	}
	if recorded, err := a.FinishSkinJob(ctx, skinJobID('b'), "succeeded", "", []byte(`{}`)); err != nil || recorded {
		t.Fatal("stale job revived", recorded, err)
	}

	if alive, err := a.SkinJobHeartbeat(ctx, skinJobID('c')); err != nil || !alive {
		t.Fatal(alive, err)
	}
	if recorded, err := a.FinishSkinJob(ctx, skinJobID('c'), "succeeded", "", []byte(`{"width":1}`)); err != nil || !recorded {
		t.Fatal(recorded, err)
	}
	if job, err := a.GetSkinJob(ctx, skinJobID('c'), "owner-2"); err != nil || job.State != "succeeded" || job.Reason != "" || string(job.Artwork) != `{"width":1}` {
		t.Fatal(job, err)
	}

	if _, err := db.pool.Exec(ctx, `UPDATE skin_jobs SET expires_at=now()-interval '1 second' WHERE id<>$1`, skinJobID('d')); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetSkinJob(ctx, skinJobID('c'), "owner-2"); !errors.Is(err, ErrSkinJobNotFound) {
		t.Fatal("expired job visible", err)
	}
	db.Prune(ctx)
	var ids []string
	rows, err := db.pool.Query(ctx, `SELECT id FROM skin_jobs`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || len(ids) != 1 || ids[0] != skinJobID('d') {
		t.Fatal("prune", ids, rows.Err())
	}
}

// A replica that dies leaves rows no worker will finish: a running job it was executing and a job a DELETE on another replica had flagged cancelled. Once their heartbeat is stale neither holds a slot under the caps, and the cancelled one, which no client can see or delete any more, is removed; the running one stays readable as failed until it expires.
func TestSkinJobStoreDeadWorkersReleaseSlots(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []byte{'a', 'b'} {
		if _, _, err := a.CreateSkinJob(ctx, skinJobID(c), "owner-1", 2, 2, 10, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DeleteSkinJob(ctx, skinJobID('a'), "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('c'), "owner-2", 2, 2, 10, time.Minute); !errors.Is(err, ErrSkinJobsBusy) {
		t.Fatal("live jobs must hold their slots", err)
	}
	// The replica running both jobs dies before its next heartbeat.
	if _, err := db.pool.Exec(ctx, `UPDATE skin_jobs SET heartbeat_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('c'), "owner-1", 2, 2, 10, time.Minute); err != nil {
		t.Fatal("dead jobs held the per-owner cap", err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('d'), "owner-2", 2, 2, 10, time.Minute); err != nil {
		t.Fatal("dead jobs held the deployment cap", err)
	}
	var cancelled int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM skin_jobs WHERE id=$1`, skinJobID('a')).Scan(&cancelled); err != nil || cancelled != 0 {
		t.Fatal("orphaned cancelled job kept", cancelled, err)
	}
	if job, err := a.GetSkinJob(ctx, skinJobID('b'), "owner-1"); err != nil || job.State != "failed" || job.Reason != "cancelled" {
		t.Fatal("orphaned running job", job, err)
	}
	if _, _, err := a.CreateSkinJob(ctx, skinJobID('e'), "owner-2", 2, 2, 10, time.Minute); !errors.Is(err, ErrSkinJobsBusy) {
		t.Fatal("live jobs must still fill the caps", err)
	}
}

// 任务已经过期时，即使最后一次心跳仍然新鲜，也不能接受迟到的成功或失败结果。
func TestSkinJobStoreRejectsExpiredResults(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := t.Context()
	if _, err := db.pool.Exec(ctx, `TRUNCATE skin_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			id := skinJobID('a')
			if _, _, err := a.CreateSkinJob(ctx, id, "owner", 1, 1, 10, time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `UPDATE skin_jobs SET expires_at=now()-interval '1 second',heartbeat_at=now() WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			if alive, err := a.SkinJobHeartbeat(ctx, id); err != nil || alive {
				t.Fatal("过期任务仍允许继续执行", alive, err)
			}
			var artwork []byte
			reason := "upstream_failure"
			if state == "succeeded" {
				artwork = []byte(`{"width":1}`)
				reason = ""
			}
			if recorded, err := a.FinishSkinJob(ctx, id, state, reason, artwork); err != nil || recorded {
				t.Error("过期任务仍接受结果", recorded, err)
			}
			var storedState, storedReason string
			var storedArtwork []byte
			if err := db.pool.QueryRow(ctx, `SELECT state,reason,artwork FROM skin_jobs WHERE id=$1`, id).Scan(&storedState, &storedReason, &storedArtwork); err != nil {
				t.Fatal(err)
			}
			if storedState != "running" || storedReason != "" || storedArtwork != nil {
				t.Errorf("迟到结果改变了过期记录：状态=%q，原因=%q，插画=%s", storedState, storedReason, storedArtwork)
			}
			if _, err := a.GetSkinJob(ctx, id, "owner"); !errors.Is(err, ErrSkinJobNotFound) {
				t.Fatal("过期任务仍可读取", err)
			}
			reservation, _, err := a.CreateSkinJob(ctx, skinJobID('b'), "owner", 1, 1, 10, time.Minute)
			if err != nil {
				t.Fatal("过期任务未释放容量", err)
			}
			if err := a.DropSkinJob(ctx, reservation); err != nil {
				t.Fatal(err)
			}
		})
	}
}
