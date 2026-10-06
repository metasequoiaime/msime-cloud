package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// 每个 owner 每天最多 skinJobsPerDay 个插画任务，删掉的任务也算数；超出后 429 并带 Retry-After，别的 owner 不受影响，额度跨副本共享。
func TestSharedSkinJobDailyQuota(t *testing.T) {
	a, b, _, _ := sharedSkinReplicas(t, 4, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
	})
	for i := range skinJobsPerDay {
		s := a
		if i%2 == 1 {
			s = b
		}
		path := createArtworkJob(t, s)
		if w := call(s, "DELETE", path, ""); w.Code != 204 {
			t.Fatal("delete", w.Code)
		}
		waitSkinWorkersIdle(t, s)
	}
	for _, s := range []*Server{a, b} {
		w := call(s, "POST", "/v1/skins/jobs", `{"prompt":"one more"}`)
		retry, _ := strconv.Atoi(w.Header().Get("Retry-After"))
		if w.Code != 429 || !bytes.Contains(w.Body.Bytes(), []byte(`"rate_limit_exceeded"`)) || retry < 1 || retry > 86400 {
			t.Fatal("daily quota", w.Code, w.Header(), w.Body.String())
		}
	}
	if w := callAs(b, otherSkinToken, "POST", "/v1/skins/jobs", `{"prompt":"other"}`); w.Code != 202 {
		t.Fatal("other owner", w.Code, w.Body.String())
	}
}

// waitSkinWorkersIdle 等这个副本上被取消的任务的 worker 真正收尾：worker 删掉行之前，任务仍占着 owner 的并发名额（skinJobsPerOwner），紧接着再建可能先拿到 503 skin_jobs_busy，-race 下 worker 收尾更慢，CI 上因此偶发失败。worker 在删行之后才把任务从 skinRunning 里去掉，所以等它空了就够。只看副本内存里的状态、不发请求：之前用 POST 轮询重试，每次都消耗客户端每分钟的请求额度，worker 慢的时候会把额度耗尽，得到 429 rate_limit_exceeded。
func waitSkinWorkersIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		running := len(s.skinRunning)
		s.mu.Unlock()
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d skin artwork workers still running", running)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
