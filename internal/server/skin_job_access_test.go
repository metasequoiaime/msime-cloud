package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSkinJobAccessBoundaries(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete} {
		for _, tc := range []struct {
			name    string
			owner   string
			id      string
			expired bool
			status  int
			pruned  bool
		}{
			{name: "本人", owner: "owner", status: http.StatusOK},
			{name: "其他用户", owner: "other", status: http.StatusNotFound},
			{name: "缺少用户", status: http.StatusNotFound},
			{name: "过期", owner: "owner", expired: true, status: http.StatusNotFound, pruned: true},
			{name: "缺少用户仍清理过期任务", expired: true, status: http.StatusNotFound, pruned: true},
			{name: "无效标识不清理任务", owner: "owner", id: "invalid", expired: true, status: http.StatusNotFound},
			{name: "不存在", owner: "owner", id: strings.Repeat("b", 48), status: http.StatusNotFound},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				id := strings.Repeat("a", 48)
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				expires := time.Now().Add(time.Hour)
				if tc.expired {
					expires = time.Now().Add(-time.Hour)
				}
				s := &Server{skinJobs: map[string]*skinArtworkJob{id: {owner: "owner", expires: expires, cancel: cancel, state: "running"}}}
				pathID := tc.id
				if pathID == "" {
					pathID = id
				}
				r := httptest.NewRequest(method, "/v1/skins/jobs/"+pathID, nil)
				r.SetPathValue("job", pathID)
				r = r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, tc.owner))
				w := httptest.NewRecorder()
				s.skinArtworkJob(w, r)
				status := tc.status
				deleted := method == http.MethodDelete && status == http.StatusOK
				if deleted {
					status = http.StatusNoContent
				}
				if w.Code != status {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				removed := tc.pruned || deleted
				if (s.skinJobs[id] == nil) != removed || (ctx.Err() != nil) != removed {
					t.Fatalf("任务删除或取消不符合预期: removed=%t cancelled=%t", s.skinJobs[id] == nil, ctx.Err() != nil)
				}
				if status == http.StatusOK {
					if w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), `"state":"running"`) || !strings.Contains(w.Body.String(), `"artwork":null`) {
						t.Fatalf("运行状态响应不正确: header=%v body=%s", w.Header(), w.Body.String())
					}
				} else if w.Header().Get("Retry-After") != "" {
					t.Fatal("错误或删除响应包含轮询间隔")
				}
				if status == http.StatusNotFound && !strings.Contains(w.Body.String(), `"code":"skin_job_not_found"`) {
					t.Fatal(w.Body.String())
				}
				if status == http.StatusNoContent && w.Body.Len() != 0 {
					t.Fatal("删除响应不应包含正文")
				}
			})
		}
	}
}

func TestSkinJobAccessFinishedPayload(t *testing.T) {
	for _, state := range []string{"succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			id := strings.Repeat("a", 48)
			job := &skinArtworkJob{owner: "owner", expires: time.Now().Add(time.Hour), cancel: func() {}, state: state}
			if state == "succeeded" {
				job.artwork = json.RawMessage(`{"width":32}`)
			} else {
				job.reason = "upstream_timeout"
			}
			s := &Server{skinJobs: map[string]*skinArtworkJob{id: job}}
			r := httptest.NewRequest(http.MethodGet, "/v1/skins/jobs/"+id, nil)
			r.SetPathValue("job", id)
			r = r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, "owner"))
			w := httptest.NewRecorder()
			s.skinArtworkJob(w, r)
			var payload map[string]json.RawMessage
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &payload) != nil || w.Header().Get("Retry-After") != "" {
				t.Fatalf("status=%d header=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			if state == "succeeded" {
				if string(payload["artwork"]) != string(job.artwork) || payload["reason"] != nil {
					t.Fatal(w.Body.String())
				}
			} else if string(payload["reason"]) != `"upstream_timeout"` || string(payload["artwork"]) != "null" {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestSkinJobAccessHeadRoute(t *testing.T) {
	s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("查询不应访问上游") })
	t.Cleanup(s.Close)
	id := strings.Repeat("a", 48)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.skinJobs = map[string]*skinArtworkJob{id: {owner: "test", expires: time.Now().Add(time.Hour), cancel: cancel, state: "running"}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	r, err := http.NewRequest(http.MethodHead, srv.URL+"/v1/skins/jobs/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Retry-After") != "5" || len(body) != 0 {
		t.Fatalf("status=%d header=%v body=%s err=%v", response.StatusCode, response.Header, body, err)
	}
	if ctx.Err() != nil || call(s, http.MethodGet, "/v1/skins/jobs/"+id, "").Code != http.StatusOK {
		t.Fatal("HEAD 不应取消或删除任务")
	}
}
