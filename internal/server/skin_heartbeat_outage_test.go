package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestSharedSkinJobConfirmsLeaseBeforeUpstream(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "missing"
		if unavailable {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			a, _, _, _ := sharedSkinReplicas(t, 1, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "synthetic upstream failure", http.StatusBadGateway)
			})
			if unavailable {
				a.CloseAccounts()
			}
			ctx, cancel := context.WithCancel(a.lifetime)
			defer cancel()
			a.skinWorkers.Add(1)
			a.runSharedSkinJob(ctx, cancel, strings.Repeat("a", 48), []byte(`{"prompt":"synthetic artwork"}`))
			if got := calls.Load(); got != 0 {
				t.Fatal("未确认任务租约就发起了上游请求", got)
			}
		})
	}
}

func TestSharedSkinJobSurvivesBriefHeartbeatFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24))); err != nil {
		t.Fatal(err)
	}
	a, b, admin, schema := sharedSkinReplicas(t, 1, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		respond(w, http.StatusOK, map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(encoded.Bytes())}}})
	})
	path := createArtworkJob(t, a)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("插画上游请求没有启动")
	}
	table := pgx.Identifier{schema, "skin_jobs"}.Sanitize()
	sequence := pgx.Identifier{schema, "skin_heartbeat_failures"}.Sanitize()
	function := pgx.Identifier{schema, "reject_skin_heartbeat"}.Sanitize()
	// 序列递增不随失败的事务回滚，用它确认工作进程确实遇到了心跳错误。
	_, err := admin.Exec(t.Context(), fmt.Sprintf(`CREATE SEQUENCE %s;
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 PERFORM nextval(TG_ARGV[0]::regclass);
 RAISE EXCEPTION 'synthetic heartbeat failure';
END $$;
CREATE TRIGGER reject_skin_heartbeat BEFORE UPDATE OF heartbeat_at ON %s
 FOR EACH ROW EXECUTE FUNCTION %s('%s')`, sequence, function, table, function, sequence))
	if err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := admin.QueryRow(t.Context(), "SELECT heartbeat_at FROM "+table+" WHERE id=$1", strings.TrimPrefix(path, "/v1/skins/jobs/")).Scan(&before); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "未观察到实际心跳错误", func() bool {
		var failed bool
		if err := admin.QueryRow(t.Context(), "SELECT is_called FROM "+sequence).Scan(&failed); err != nil {
			t.Fatal(err)
		}
		return failed
	})
	if _, err := admin.Exec(t.Context(), "DROP TRIGGER reject_skin_heartbeat ON "+table); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "短暂故障恢复后没有续约", func() bool {
		var heartbeat time.Time
		if err := admin.QueryRow(t.Context(), "SELECT heartbeat_at FROM "+table+" WHERE id=$1", strings.TrimPrefix(path, "/v1/skins/jobs/")).Scan(&heartbeat); err != nil {
			t.Fatal(err)
		}
		return heartbeat.After(before)
	})
	// 越过原租约的期限，确认成功续约确实重置了本地计时器。
	ctx, cancel := context.WithTimeout(t.Context(), account.SkinJobStaleAfter+10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var elapsed bool
		if err := admin.QueryRow(ctx, "SELECT clock_timestamp()>$1", before.Add(account.SkinJobStaleAfter)).Scan(&elapsed); err != nil {
			t.Fatal(err)
		}
		if elapsed {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("等待原租约期限超时", ctx.Err())
		}
	}
	close(release)
	a.skinWorkers.Wait()
	if w := call(b, "GET", path, ""); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"state":"succeeded"`)) {
		t.Fatal("短暂心跳错误中断了正常插画任务", w.Code, w.Body.String())
	}
}

func TestSharedSkinJobStopsWhenHeartbeatStoreStaysUnavailable(t *testing.T) {
	entered, stopped := make(chan struct{}), make(chan struct{})
	a, b, _, _ := sharedSkinReplicas(t, 1, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(stopped)
	})
	path := createArtworkJob(t, a)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("插画上游请求没有启动")
	}
	// 只断开执行副本的连接池，另一个副本仍可按真实数据库心跳判断失效。
	a.CloseAccounts()
	ctx, cancel := context.WithTimeout(t.Context(), account.SkinJobStaleAfter+10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		w := call(b, "GET", path, "")
		if w.Code != http.StatusOK {
			t.Fatal("健康副本无法查询任务", w.Code, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte(`"state":"failed"`)) {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("健康副本没有将失去心跳的任务判为失败", ctx.Err())
		}
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("任务已失效并释放部署容量，断开数据库的副本仍在请求付费上游")
	}
	a.skinWorkers.Wait()
}
