package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type dictionaryMockRequest struct {
	method, path string
	body         map[string]any
}
type dictionaryMock struct {
	mu                   sync.Mutex
	requests             []dictionaryMockRequest
	failMethod, failPath string
	failStatus           int
	text                 string
	tree                 []dictionaryTreeItem
	blobs                map[string]string
	truncated            bool
	pullCreated          bool
}

func (f *dictionaryMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.requests = append(f.requests, dictionaryMockRequest{r.Method, r.URL.Path, body})
	if r.Method == f.failMethod && r.URL.Path == f.failPath {
		w.WriteHeader(f.failStatus)
		_, _ = w.Write([]byte(`{"message":"private upstream detail"}`))
		return
	}
	root := "/repos/" + dictTestRepo
	sha := dictionaryBlobSHA([]byte(f.text))
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
	case r.Method == "GET" && r.URL.Path == root:
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main", "html_url": "https://github.com/" + dictTestRepo, "description": "合成词库", "pushed_at": "2026-10-01T00:00:00Z"})
	case r.Method == "GET" && (r.URL.Path == root+"/git/trees/main" || r.URL.Path == root+"/git/trees/"+strings.Repeat("c", 40)):
		tree := any(f.tree)
		if f.tree == nil {
			tree = []map[string]any{
				{"path": "custom/words.txt", "sha": sha, "size": len(f.text), "type": "blob"},
				{"path": "packs/demo/quanpin.txt", "sha": sha, "size": len(f.text), "type": "blob"},
				{"path": "sources/example/data.txt", "sha": sha, "size": len(f.text), "type": "blob"},
				{"path": "packs/README.md", "sha": sha, "size": 20, "type": "blob"},
				{"path": "packs/demo/README.md", "sha": sha, "size": 20, "type": "blob"},
				{"path": "sources/japanese/README.txt", "sha": sha, "size": 20, "type": "blob"},
				{"path": "sources/japanese/LICENSE", "sha": sha, "size": 20, "type": "blob"},
				{"path": "sources/japanese/connection_single_column.txt", "sha": sha, "size": 20, "type": "blob"},
				{"path": "sources/japanese/id.def", "sha": sha, "size": 20, "type": "blob"},
				{"path": ".github/workflows/check.yml", "sha": sha, "size": 20, "type": "blob"},
				{"path": "custom", "sha": sha, "type": "tree"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": strings.Repeat("c", 40), "tree": tree, "truncated": f.truncated})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, root+"/git/blobs/") && f.blobs != nil:
		body, ok := f.blobs[strings.TrimPrefix(r.URL.Path, root+"/git/blobs/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(body))
	case r.Method == "GET" && r.URL.Path == root+"/git/blobs/"+sha:
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(f.text))
	case r.Method == "GET" && r.URL.Path == root+"/commits/main":
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": strings.Repeat("b", 40)})
	case r.Method == "GET" && r.URL.Path == root+"/pulls":
		pulls := []any{}
		if f.pullCreated {
			pulls = append(pulls, map[string]any{"number": 42, "state": "open", "head": map[string]any{"ref": "dictionary-admin/test", "repo": map[string]string{"full_name": dictTestRepo}}, "base": map[string]string{"ref": "main"}})
		}
		_ = json.NewEncoder(w).Encode(pulls)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, root+"/contents/"):
		if r.URL.Query().Get("ref") != strings.Repeat("b", 40) || r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(f.text))
	case r.Method == "POST" && r.URL.Path == root+"/git/refs":
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, root+"/contents/"):
		_, _ = w.Write([]byte(`{}`))
	case r.Method == "POST" && r.URL.Path == root+"/pulls":
		f.pullCreated = true
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/" + dictTestRepo + "/pull/42"})
	default:
		w.WriteHeader(404)
	}
}

func TestDictionaryChangesInvalidatesPullCache(t *testing.T) {
	f := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n"}
	client := dictionaryMockClient(t, f)
	client.Cache = &githubapp.Cache{}
	path := "/repos/" + dictTestRepo + "/pulls"
	if response, err := client.Get(t.Context(), "synthetic-token", path); err != nil || !response.OK() {
		t.Fatalf("预热词库 PR 缓存失败：状态=%d，错误=%v", response.Status, err)
	}
	s := &Server{adminGitHub: client, config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	body, _ := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(f.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
	if response := dictionaryDirect(s, "POST", "/api/dictionary/changes", string(body), true); response.Code != 200 {
		t.Fatalf("提交词库变更失败：状态=%d，响应=%s", response.Code, response.Body.String())
	}
	response, err := client.Get(t.Context(), "synthetic-token", path)
	if err != nil || !response.OK() {
		t.Fatalf("读取词库 PR 列表失败：状态=%d，错误=%v", response.Status, err)
	}
	var pulls []map[string]any
	if err := response.Decode(&pulls); err != nil || len(pulls) != 1 {
		t.Fatalf("提交后词库 PR 缓存未失效：数量=%d，错误=%v", len(pulls), err)
	}
}

func dictionaryMockClient(t *testing.T, f *dictionaryMock) *githubapp.Client {
	t.Helper()
	upstream := httptest.NewServer(f)
	t.Cleanup(upstream.Close)
	return &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client()}
}
func (f *dictionaryMock) writes() []dictionaryMockRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []dictionaryMockRequest{}
	for _, r := range f.requests {
		if (r.method == "POST" || r.method == "PUT") && !strings.HasSuffix(r.path, "/access_tokens") {
			out = append(out, r)
		}
	}
	return out
}
func dictionaryDirect(s *Server, method, path, body string, permitted bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	permissions := []string{}
	if permitted {
		permissions = append(permissions, account.PermReviewDictPR)
	}
	request = request.WithContext(account.WithAdminAccess(request.Context(), account.AdminAccess{Actor: "test", Permissions: permissions}))
	response := httptest.NewRecorder()
	s.adminDictionary(response, request)
	return response
}

func TestDictionaryManagementHTTP(t *testing.T) {
	f := &dictionaryMock{text: "# 说明\n甲\tjia\t1\n乙\tyi\t5\n"}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	w := dictionaryDirect(s, "GET", "/api/dictionary", "", false)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	listing := decodeDict[struct {
		Files  []dictionaryTreeItem `json:"files"`
		Counts map[string]int       `json:"counts"`
	}](t, w)
	if len(listing.Files) != 3 || listing.Counts["custom"] != 1 || listing.Counts["packs"] != 1 || listing.Counts["sources"] != 1 {
		t.Fatalf("listing=%+v", listing)
	}
	w = dictionaryDirect(s, "GET", "/api/dictionary/file?path=custom%2Fwords.txt&q=%E7%94%B2", "", false)
	file := decodeDict[struct {
		Rows     []dictionaryRow `json:"rows"`
		Total    int             `json:"total"`
		Editable bool            `json:"editable"`
	}](t, w)
	if w.Code != 200 || file.Total != 1 || !file.Editable || file.Rows[0].Columns[0] != "甲" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, bad := range []string{"/api/dictionary/file?path=../custom/words.txt", "/api/dictionary/file?path=custom/words.txt&page=0", "/api/dictionary/file?path=custom/words.txt&q=%0A", "/api/dictionary/file?path=packs/README.md", "/api/dictionary/file?path=packs/demo/README.md", "/api/dictionary/file?path=sources/japanese/README.txt", "/api/dictionary/file?path=sources/japanese/LICENSE", "/api/dictionary/file?path=sources/japanese/connection_single_column.txt"} {
		w = dictionaryDirect(s, "GET", bad, "", false)
		if w.Code != 400 {
			t.Errorf("path=%s status=%d", bad, w.Code)
		}
	}
	w = dictionaryDirect(s, "GET", "/api/dictionary/file?path=packs/missing/quanpin.txt", "", false)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = dictionaryDirect(s, "POST", "/api/dictionary", "{}", true)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = dictionaryDirect(&Server{}, "GET", "/api/dictionary", "", false)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "github_disabled") {
		t.Fatal(w.Code, w.Body.String())
	}
	body, _ := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(f.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
	w = dictionaryDirect(s, "POST", "/api/dictionary/changes", string(body), false)
	if w.Code != 403 || len(f.writes()) != 0 {
		t.Fatal(w.Code, "permission gate failed")
	}
	w = dictionaryDirect(s, "POST", "/api/dictionary/changes", string(body), true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	writes := f.writes()
	if len(writes) != 3 {
		t.Fatalf("writes=%+v", writes)
	}
	raw, err := base64.StdEncoding.DecodeString(writes[1].body["content"].(string))
	if err != nil || string(raw) != f.text+"丙\tbing\t3\n" {
		t.Fatalf("committed=%q err=%v", raw, err)
	}
	if writes[1].body["branch"] == "main" || writes[2].body["base"] != "main" {
		t.Fatalf("writes targeted wrong branch: %+v", writes)
	}
}

func TestDictionaryChangesRejectStaleAndInvalidRequests(t *testing.T) {
	f := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n"}
	s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	for _, body := range []string{
		`{}`, `{"unknown":true}`, `{} {}`, `{"path":"sources/pinyin/base.txt","sha":"abc","changes":[]}`,
		`{"path":"custom/words.txt","sha":"invalid","changes":[{"operation":"add","columns":["丙","bing","3"]}]}`,
	} {
		if w := dictionaryDirect(s, "POST", "/api/dictionary/changes", body, true); w.Code != 400 {
			t.Errorf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	body, _ := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: strings.Repeat("a", 40), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
	w := dictionaryDirect(s, "POST", "/api/dictionary/changes", string(body), true)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "dictionary_changed") || len(f.writes()) != 0 {
		t.Fatal(w.Code, w.Body.String(), f.writes())
	}
}

func TestDictionaryChangesGitHubFailures(t *testing.T) {
	root := "/repos/" + dictTestRepo
	for _, phase := range []struct {
		method, path string
		status, want int
		code         string
	}{
		{"GET", root, 503, 503, "github_unavailable"}, {"GET", root + "/commits/main", 404, 503, "github_unavailable"},
		{"POST", root + "/git/refs", 503, 502, "dictionary_uncertain"},
		{"PUT", root + "/contents/custom/words.txt", 422, 502, "github_error"},
		{"POST", root + "/pulls", 503, 502, "dictionary_uncertain"},
	} {
		t.Run(phase.method+phase.path, func(t *testing.T) {
			f := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n", failMethod: phase.method, failPath: phase.path, failStatus: phase.status}
			s := &Server{adminGitHub: dictionaryMockClient(t, f), config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
			body, _ := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(f.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
			w := dictionaryDirect(s, "POST", "/api/dictionary/changes", string(body), true)
			if w.Code != phase.want || !strings.Contains(w.Body.String(), phase.code) || strings.Contains(w.Body.String(), "private upstream") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestDictionaryChangesAuditIntegration(t *testing.T) {
	s, _, conn, schema := dictPRServer(t, "")
	f := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n"}
	s.adminGitHub = dictionaryMockClient(t, f)
	body, _ := json.Marshal(dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(f.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
	w := dictCall(s, "POST", "/api/dictionary/changes", string(body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	audit := dictAudit(t, conn, schema)
	if len(audit) != 1 || audit[0].action != "dictionary_change" || audit[0].target != "custom/words.txt" || audit[0].count != "1" {
		t.Fatalf("audit=%+v", audit)
	}
	w = dictCall(s, "GET", "/dictionary", "")
	if w.Code != 200 {
		t.Fatal("SPA direct route", w.Code)
	}
}
