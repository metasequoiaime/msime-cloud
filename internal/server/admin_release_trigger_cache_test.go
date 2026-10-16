package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseTriggerRefreshesReleaseAndCheckCaches(t *testing.T) {
	for _, state := range []string{"pending", "failed"} {
		for _, outcome := range []string{"成功响应", "连接中断", "上游错误"} {
			t.Run(state+"/"+outcome, func(t *testing.T) {
				s, f, _, _ := releaseDBServer(t)
				s.config.Admin.GitHub.Platforms = releaseTestPlatforms[:1]
				p := releaseTestPlatforms[0]
				dispatch := "/repos/" + p.Repo + "/actions/workflows/release.yml/dispatches"
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "POST" || r.URL.Path != dispatch {
						f.ServeHTTP(w, r)
						return
					}
					response := httptest.NewRecorder()
					f.ServeHTTP(response, r)
					if response.Code != 204 {
						t.Errorf("触发上游流水线失败：%d", response.Code)
					}
					// 流水线在触发响应返回前已产生新草稿和检查结果。
					status, conclusion := "in_progress", ""
					if state == "failed" {
						status, conclusion = "completed", "failure"
					}
					f.mu.Lock()
					f.runs[p.Repo+"@main"] = []map[string]any{{"name": "build", "status": status, "conclusion": conclusion}}
					f.releases[p.Repo] = append(f.releases[p.Repo], map[string]any{
						"id": 5, "tag_name": "windows-v0.5.6", "draft": true, "body": "新的发布说明", "target_commitish": "main", "created_at": "2026-10-02T08:00:00Z",
					})
					f.mu.Unlock()
					w.WriteHeader(response.Code)
				})
				var upstream *httptest.Server
				wantStatus := 502
				if outcome == "成功响应" {
					upstream = httptest.NewServer(handler)
					t.Cleanup(upstream.Close)
					wantStatus = 200
				} else {
					upstream = uncertainAdminWriteUpstream(t, handler, "POST", dispatch, outcome)
				}
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
				read := func() releaseListBody {
					var list releaseListBody
					request := httptest.NewRequest("GET", "https://admin.example.com/api/releases", nil)
					request.Header.Set("Authorization", "Bearer "+strings.Repeat("q", 48))
					serveAdminJSON(t, s, request, 200, &list)
					if len(list.Platforms) != 1 || list.Platforms[0].Latest == nil || len(list.Platforms[0].Checklist) == 0 {
						t.Fatalf("发布列表无效：%+v", list)
					}
					return list
				}
				before := read().Platforms[0]
				if before.Latest.Tag != "windows-v0.5.5" || before.Checklist[0].State != "passed" {
					t.Fatalf("预热缓存失败：%+v", before)
				}
				serveAdminJSON(t, s, releaseWrite("/api/releases/windows/trigger", "{\"version\":\"v0.5.6\"}"), wantStatus, nil)
				after := read().Platforms[0]
				if after.Latest.Tag != "windows-v0.5.6" {
					t.Errorf("触发后仍显示旧发布草稿：%s", after.Latest.Tag)
				}
				if after.Checklist[0].State != state {
					t.Errorf("触发后仍显示旧检查状态：%s，期望 %s", after.Checklist[0].State, state)
				}
			})
		}
	}
}
