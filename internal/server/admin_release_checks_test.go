package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestAdminReleaseChecklistReadsAllCheckPages(t *testing.T) {
	for _, tc := range []struct {
		name, status, conclusion, want, sign string
		count, pages                         int
	}{
		{"empty", "", "", "unknown", "", 0, 1},
		{"full first page", "completed", "success", "passed", "", 100, 1},
		{"second page failure", "completed", "failure", "failed", "failed", 101, 2},
		{"second page pending", "in_progress", "", "pending", "pending", 101, 2},
		{"full second page", "completed", "success", "passed", "passed", 200, 2},
		{"third page failure", "completed", "failure", "failed", "failed", 201, 3},
		{"page limit complete", "completed", "success", "passed", "passed", 1000, 10},
		{"page limit incomplete", "completed", "failure", "unknown", "", 1001, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := adminRBACFixture(t)
			s.config.Admin.GitHub.Platforms = releaseTestPlatforms[:1]
			f := newFakeReleaseGitHub(t)
			var runs []map[string]any
			for i := range tc.count {
				runs = append(runs, map[string]any{"name": "build-" + strconv.Itoa(i), "status": "completed", "conclusion": "success"})
			}
			if tc.count > 100 {
				runs[len(runs)-1] = map[string]any{"name": "sign", "status": tc.status, "conclusion": tc.conclusion}
			}
			f.runs[releaseTestPlatforms[0].Repo+"@main"] = runs
			s.adminGitHub = f.client(t)
			for range 2 {
				var list releaseListBody
				serveAdminJSON(t, s, adminRequest(s, "GET", "/api/releases", "", account.AdminTokenPrefix+"readonly"), 200, &list)
				if len(list.Platforms) != 1 || list.Platforms[0].Latest == nil {
					t.Fatalf("发布列表 = %+v", list)
				}
				checks := map[string]string{}
				for _, check := range list.Platforms[0].Checklist {
					checks[check.Key] = check.State
				}
				if checks["ci"] != tc.want || checks["sign"] != tc.sign {
					t.Fatalf("检查状态 = %v，期望 CI=%s，sign=%s", checks, tc.want, tc.sign)
				}
			}
			if calls := f.count("GET /repos/" + releaseTestPlatforms[0].Repo + "/commits/main/check-runs"); calls != tc.pages {
				t.Fatalf("读取检查页数 = %d，期望 %d；第二次读取应命中缓存", calls, tc.pages)
			}
		})
	}
}

func TestReleaseChecklistRejectsIncompleteCheckPages(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"upstream failure", `{}`, 503},
		{"invalid JSON", `{`, 200},
		{"missing page", `{"total_count":101,"check_runs":[]}`, 200},
		{"changed count", `{"total_count":102,"check_runs":[{"name":"sign","status":"completed","conclusion":"success"}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReleaseGitHub(t)
			p := releaseTestPlatforms[0]
			var runs []map[string]any
			for i := range 101 {
				runs = append(runs, map[string]any{"name": "build-" + strconv.Itoa(i), "status": "completed", "conclusion": "success"})
			}
			f.runs[p.Repo+"@main"] = runs
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/"+p.Repo+"/commits/main/check-runs" && r.URL.Query().Get("page") == "2" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			client := f.client(t)
			client.APIURL, client.HTTP = upstream.URL, upstream.Client()
			checks := releaseChecklist(t.Context(), client, p, ghRelease{TagName: "main"}, releaseView{})
			if checks[0].State != "unknown" {
				raw, _ := json.Marshal(checks)
				t.Fatalf("未完整读取检查时的清单 = %s", raw)
			}
		})
	}
}
