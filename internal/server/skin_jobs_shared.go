package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// skinJobHeartbeat is how often a worker refreshes its job's heartbeat in skin_jobs and, in the same statement, learns whether a DELETE on another replica cancelled it.
//
// Polling the row was chosen over LISTEN/NOTIFY: the heartbeat has to reach the database anyway so that other replicas can tell a dead worker from a slow one, and folding the cancel check into it costs no extra query. LISTEN would need a dedicated connection per replica held outside the pool, reconnect and re-listen handling, and a fallback poll for notifications lost while disconnected — more moving parts to save at most a few seconds on a 180-second job whose client polls every 5 seconds. A DELETE that lands on the executing replica still cancels at once through skinRunning.
const skinJobHeartbeat = 5 * time.Second

// skinJobWriteTimeout bounds the worker's own database writes. They run on a fresh context because the job's context is already done when a shutdown or cancellation ends it, and the outcome must still be written.
const skinJobWriteTimeout = 5 * time.Second

func (s *Server) skinJobsBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	fail(w, 503, "skin_jobs_busy")
}

func skinJobUnavailable(w http.ResponseWriter, err error) {
	slog.Error("skin artwork job store unavailable", "reason", err.Error())
	w.Header().Set("Retry-After", "5")
	fail(w, 503, "job_unavailable")
}

// createSharedSkinJob records the job in skin_jobs, where the per-owner and deployment-wide caps are checked under one advisory lock, and runs it on this replica.
func (s *Server) createSharedSkinJob(w http.ResponseWriter, r *http.Request, id, owner string, body []byte) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		s.skinJobsBusy(w)
		return
	}
	reservation, wait, err := s.accounts.CreateSkinJob(r.Context(), id, owner, skinJobsPerOwner, skinJobCap(s.config), skinJobsPerDay, skinJobTTL)
	if errors.Is(err, account.ErrLimited) {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		fail(w, 429, "rate_limit_exceeded")
		return
	}
	if errors.Is(err, account.ErrSkinJobsBusy) {
		s.skinJobsBusy(w)
		return
	}
	if errors.Is(err, account.ErrBanned) {
		fail(w, http.StatusForbidden, "account_banned")
		return
	}
	if errors.Is(err, account.ErrInvalid) {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		skinJobUnavailable(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(s.lifetime, skinJobTimeout)
	s.mu.Lock()
	if s.closed {
		// 登记后开始关闭，`Close` 可能已在等待工作进程，不能再增加任务；撤回任务并退还额度。
		s.mu.Unlock()
		cancel()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), skinJobWriteTimeout)
		if err := s.accounts.DropSkinJob(dropCtx, reservation); err != nil {
			slog.Warn("unstarted skin artwork job not dropped", "job", id, "reason", err.Error())
		}
		dropCancel()
		s.skinJobsBusy(w)
		return
	}
	if s.skinRunning == nil {
		s.skinRunning = make(map[string]context.CancelFunc)
	}
	s.skinRunning[id] = cancel
	s.skinWorkers.Add(1)
	s.mu.Unlock()
	go s.runSharedSkinJob(ctx, cancel, id, body)
	respondSkinJobCreated(w, id, reservation.ExpiresAt)
}

func (s *Server) runSharedSkinJob(ctx context.Context, cancel context.CancelFunc, id string, body []byte) {
	defer s.skinWorkers.Done()
	defer cancel()
	defer func() {
		s.mu.Lock()
		delete(s.skinRunning, id)
		s.mu.Unlock()
	}()
	renew := func() (bool, error, time.Time) {
		// 从请求开始计时，响应等待不能延长数据库已经确认的心跳期限。
		deadline := time.Now().Add(account.SkinJobStaleAfter)
		beatCtx, beatCancel := context.WithTimeout(ctx, skinJobWriteTimeout)
		defer beatCancel()
		alive, err := s.accounts.SkinJobHeartbeat(beatCtx, id)
		return alive, err, deadline
	}
	beating := make(chan struct{})
	alive, err, deadline := renew()
	if err != nil || !alive {
		// 首次心跳确认成功后才访问付费上游，避免启动前已经失效或断开数据库。
		cancel()
		close(beating)
	} else {
		// 独立计时器在数据库请求阻塞或持续报错时也会停止上游；只有成功续约才能重置。
		lease := time.AfterFunc(time.Until(deadline), cancel)
		go func() {
			defer close(beating)
			defer lease.Stop()
			ticker := time.NewTicker(s.skinHeartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				alive, err, deadline := renew()
				if err != nil {
					continue
				}
				if !alive {
					cancel()
					return
				}
				lease.Reset(time.Until(deadline))
			}
		}()
	}
	response := s.runSkinArtwork(ctx, body)
	ctxErr := ctx.Err()
	cancel()
	<-beating
	state, reason := "succeeded", ""
	var artwork []byte
	if response.status == http.StatusOK && ctxErr == nil {
		artwork = response.body.Bytes()
	} else {
		state, reason = "failed", skinFailureReason(response, ctxErr)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), skinJobWriteTimeout)
	defer writeCancel()
	recorded, err := s.accounts.FinishSkinJob(writeCtx, id, state, reason, artwork)
	if err != nil {
		// The heartbeat stops with the worker, so the job goes stale and every replica reports it failed within account.SkinJobStaleAfter.
		slog.Error("skin artwork job result not recorded", "job", id, "state", state, "reason", err.Error())
		return
	}
	if recorded && state == "failed" {
		slog.Error("skin artwork job failed", "job", id, "status", response.status, "reason", reason)
	}
}

func (s *Server) getSharedSkinJob(w http.ResponseWriter, r *http.Request, owner string) {
	if owner == "" {
		fail(w, 404, "skin_job_not_found")
		return
	}
	job, err := s.accounts.GetSkinJob(r.Context(), r.PathValue("job"), owner)
	if errors.Is(err, account.ErrSkinJobNotFound) {
		fail(w, 404, "skin_job_not_found")
		return
	}
	if err != nil {
		skinJobUnavailable(w, err)
		return
	}
	respondSkinJob(w, r.PathValue("job"), job.State, job.Reason, json.RawMessage(job.Artwork))
}

func (s *Server) deleteSharedSkinJob(w http.ResponseWriter, r *http.Request, owner string) {
	if owner == "" {
		fail(w, 404, "skin_job_not_found")
		return
	}
	id := r.PathValue("job")
	err := s.accounts.DeleteSkinJob(r.Context(), id, owner)
	if errors.Is(err, account.ErrSkinJobNotFound) {
		fail(w, 404, "skin_job_not_found")
		return
	}
	if err != nil {
		skinJobUnavailable(w, err)
		return
	}
	// The job may be running on this replica: stop it now instead of at its next heartbeat.
	s.mu.Lock()
	if stop := s.skinRunning[id]; stop != nil {
		stop()
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
