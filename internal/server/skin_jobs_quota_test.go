package server

import (
	"bytes"
	"encoding/json"
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
		path := createArtworkJobOnceFree(t, s)
		if w := call(s, "DELETE", path, ""); w.Code != 204 {
			t.Fatal("delete", w.Code)
		}
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

// createArtworkJobOnceFree 等前面被 DELETE 的任务的 worker 真正停下再建：被取消的任务在 worker 删掉行之前仍占着 owner 的并发名额（skinJobsPerOwner），连续「建→删」时下一次可能先拿到 503 skin_jobs_busy。-race 下 worker 收尾更慢，CI 上因此偶发失败；这里只在 busy 时稍等重试，日配额的断言不变。
func createArtworkJobOnceFree(t *testing.T, s *Server) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := call(s, "POST", "/v1/skins/jobs", `{"prompt":"原创森林"}`)
		if w.Code == 503 && bytes.Contains(w.Body.Bytes(), []byte(`"skin_jobs_busy"`)) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if w.Code != 202 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.ID) != 48 {
			t.Fatal("missing job ID")
		}
		return "/v1/skins/jobs/" + out.ID
	}
}
