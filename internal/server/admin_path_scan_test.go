package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func adminPathTestServer() *Server {
	s := &Server{}
	s.config.Admin.GitHub.IssueRepos = []string{"owner/repo"}
	s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "windows", TagPrefix: "windows-v"}}
	return s
}

func TestAdminPathCompatibility(t *testing.T) {
	s := adminPathTestServer()
	for _, tc := range []struct {
		path, code string
		status     int
	}{
		{"owner/repo/1", "", 0},
		{"OWNER/REPO/2", "", 0},
		{"owner/repo/0", "invalid_id", 400},
		{"owner/repo/01", "invalid_id", 400},
		{"owner/repo/-1", "invalid_id", 400},
		{"owner/repo/", "invalid_id", 400},
		{"owner/repo/1/", "not_found", 404},
		{"owner/repo/01/extra", "not_found", 404},
		{"owner/repo", "not_found", 404},
		{"/owner/repo/1", "not_found", 404},
		{"owner//1", "not_found", 404},
		{"other/repo/1", "not_found", 404},
	} {
		ref, err := s.parseIssuePath(tc.path)
		if tc.code == "" {
			if err != nil || ref.Repo != "owner/repo" || ref.N < 1 {
				t.Fatalf("Issue 路径 %q：%+v、%v", tc.path, ref, err)
			}
		} else if e, ok := err.(*issueError); !ok || e.status != tc.status || e.code != tc.code {
			t.Fatalf("Issue 路径 %q：%v，期望 %d %s", tc.path, err, tc.status, tc.code)
		}
	}
	for _, tc := range []struct {
		method, path, code string
		status             int
		handler            func(http.ResponseWriter, *http.Request)
	}{
		{"GET", "/api/dict-prs/1", "github_disabled", 404, s.adminDictPRs},
		{"POST", "/api/dict-prs/1", "method_not_allowed", 405, s.adminDictPRs},
		{"GET", "/api/dict-prs/1/approve", "method_not_allowed", 405, s.adminDictPRs},
		{"POST", "/api/dict-prs/1/reject", "github_disabled", 404, s.adminDictPRs},
		{"POST", "/api/dict-prs/1/trim", "github_disabled", 404, s.adminDictPRs},
		{"POST", "/api/dict-prs/01/extra/x", "invalid_id", 400, s.adminDictPRs},
		{"POST", "/api/dict-prs/1/approve/extra", "not_found", 404, s.adminDictPRs},
		{"GET", "/api/dict-prs/1/", "not_found", 404, s.adminDictPRs},
		{"GET", "/api/releases", "github_disabled", 404, s.adminReleases},
		{"GET", "/api/releases/", "not_found", 404, s.adminReleases},
		{"POST", "/api/releases/windows", "method_not_allowed", 405, s.adminReleases},
		{"GET", "/api/releases/windows", "github_disabled", 404, s.adminReleases},
		{"GET", "/api/releases/windows/trigger", "method_not_allowed", 405, s.adminReleases},
		{"POST", "/api/releases/windows/windows-v1/notes", "github_disabled", 404, s.adminReleases},
		{"GET", "/api/releases/windows/windows-v1/a/b/notes", "method_not_allowed", 405, s.adminReleases},
		{"GET", "/api/releases/windows/windows-v1%2Fa%2Fb/withdraw", "method_not_allowed", 405, s.adminReleases},
		{"GET", "/api/releases/windows/windows-v1//notes", "method_not_allowed", 405, s.adminReleases},
		{"GET", "/api/releases/windows//notes", "invalid_tag", 400, s.adminReleases},
		{"GET", "/api/releases/windows/trigger/notes", "invalid_tag", 400, s.adminReleases},
		{"GET", "/api/releases/windows/notes", "not_found", 404, s.adminReleases},
		{"GET", "/api/releases/windows/windows-v1/notes/", "not_found", 404, s.adminReleases},
		{"GET", "/api/releases/windows/windows-v1/unknown", "not_found", 404, s.adminReleases},
		{"GET", "/api/releases/unknown/windows-v1/notes", "not_found", 404, s.adminReleases},
	} {
		w := httptest.NewRecorder()
		tc.handler(w, httptest.NewRequest(tc.method, tc.path, nil))
		var body struct{ Error struct{ Code string } }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != tc.status || body.Error.Code != tc.code {
			t.Fatalf("%s %s：%d %s，期望 %d %s", tc.method, tc.path, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}
}

func TestAdminPathAllocationBudget(t *testing.T) {
	s := adminPathTestServer()
	deep := strings.Repeat("extra/", 8192)
	var issueErr error
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, issueErr = s.parseIssuePath(deep)
		}
	})
	if e, ok := issueErr.(*issueError); !ok || e.code != "not_found" {
		t.Fatal("超深 Issue 路径未被拒绝：", issueErr)
	}
	t.Logf("Issue 路径：%s，%s", result.String(), result.MemString())
	if result.AllocedBytesPerOp() > 4096 {
		t.Errorf("Issue 路径每次分配 %d 字节，超过 4096 字节预算", result.AllocedBytesPerOp())
	}
	for _, tc := range []struct {
		name, path, code string
		status           int
		handler          func(http.ResponseWriter, *http.Request)
	}{
		{"词条 PR", "/api/dict-prs/1/" + deep, "not_found", 404, s.adminDictPRs},
		{"未知发布平台", "/api/releases/unknown/" + deep, "not_found", 404, s.adminReleases},
		{"超长发布标签", "/api/releases/windows/windows-v1/" + deep + "notes", "invalid_tag", 400, s.adminReleases},
		{"未知发布操作", "/api/releases/windows/windows-v1/" + deep + "unknown", "not_found", 404, s.adminReleases},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		var w *httptest.ResponseRecorder
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				w = httptest.NewRecorder()
				tc.handler(w, r)
			}
		})
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
		t.Logf("%s：%s，%s", tc.name, result.String(), result.MemString())
		if result.AllocedBytesPerOp() > 4096 {
			t.Errorf("%s 每次分配 %d 字节，超过 4096 字节预算", tc.name, result.AllocedBytesPerOp())
		}
	}
}
