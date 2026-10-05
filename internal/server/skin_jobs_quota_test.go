package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"
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
