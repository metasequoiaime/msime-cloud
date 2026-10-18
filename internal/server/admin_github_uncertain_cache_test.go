package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// 上游先完成写入，再丢失或破坏响应，复现客户端无法确认写入结果的情况。
func uncertainAdminWriteUpstream(t *testing.T, handler http.Handler, method, path, outcome string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path {
			handler.ServeHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code < 200 || response.Code >= 300 {
			t.Errorf("模拟故障前的上游写入失败：%d %s", response.Code, response.Body.String())
		}
		switch outcome {
		case "连接中断":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case "响应截断":
			w.Header().Set("Content-Length", "16")
			w.WriteHeader(response.Code)
			_, _ = w.Write([]byte("{"))
		case "上游错误":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "响应损坏":
			w.WriteHeader(response.Code)
			_, _ = w.Write([]byte("{"))
		case "缺少标识":
			w.WriteHeader(response.Code)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestAdminReleaseUncertainWriteInvalidatesCache(t *testing.T) {
	for _, action := range []string{"notes", "withdraw"} {
		for _, outcome := range []string{"连接中断", "上游错误"} {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				s, f, _, _ := releaseDBServer(t)
				p := releaseTestPlatforms[0]
				upstream := uncertainAdminWriteUpstream(t, f, "PATCH", "/repos/"+p.Repo+"/releases/3", outcome)
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
				if _, err := fetchPlatformReleases(t.Context(), s.adminGitHub, p, false); err != nil {
					t.Fatal(err)
				}
				body := ""
				if action == "notes" {
					body = "{\"body\":\"新的发布说明\"}"
				}
				serveAdminJSON(t, s, releaseWrite("/api/releases/windows/windows-v0.5.4/"+action, body), 502, nil)
				releases, err := fetchPlatformReleases(t.Context(), s.adminGitHub, p, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, release := range releases {
					if release.ID == 3 {
						if action == "notes" && release.Body != "新的发布说明" || action == "withdraw" && releaseStatus(release) != "withdrawn" {
							t.Fatalf("写入结果不确定时仍返回旧缓存：正文=%q 状态=%s", release.Body, releaseStatus(release))
						}
						return
					}
				}
				t.Fatal("发布列表缺少已写入的版本")
			})
		}
	}
}

func TestDictionaryUncertainWriteInvalidatesPullCache(t *testing.T) {
	for _, outcome := range []string{"连接中断", "上游错误", "响应损坏", "缺少标识"} {
		t.Run(outcome, func(t *testing.T) {
			f := &dictionaryMock{text: "甲\tjia\t1\n乙\tyi\t5\n"}
			client := dictionaryMockClient(t, f)
			client.Cache = &githubapp.Cache{}
			path := "/repos/" + dictTestRepo + "/pulls"
			upstream := uncertainAdminWriteUpstream(t, f, "POST", path, outcome)
			client.APIURL, client.HTTP = upstream.URL, upstream.Client()
			if response, err := client.Get(t.Context(), "synthetic-token", path); err != nil || !response.OK() {
				t.Fatalf("预热缓存失败：%d %v", response.Status, err)
			}
			s := &Server{config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
			_, err := s.createDictionaryPR(t.Context(), client, dictionaryChangeRequest{Path: "custom/words.txt", SHA: dictionaryBlobSHA([]byte(f.text)), Changes: []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}})
			if err == nil || err.Error() != "dictionary_uncertain" {
				t.Fatalf("应报告写入结果不确定：%v", err)
			}
			response, err := client.Get(t.Context(), "synthetic-token", path)
			var pulls []map[string]any
			if err != nil || response.Decode(&pulls) != nil || len(pulls) != 1 {
				t.Fatalf("写入结果不确定时仍返回旧 PR 列表：%s %v", response.Body, err)
			}
		})
	}
}

func TestCrashUncertainWriteInvalidatesIssueCache(t *testing.T) {
	for _, outcome := range []string{"连接中断", "上游错误", "响应损坏", "缺少标识"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, db, schema := releaseDBServer(t)
			const signature = "0123456789abcdef"
			if _, err := db.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{schema, "admin_crash_groups"}.Sanitize()+"(signature,platform,version,title) VALUES($1,'ios','1.0.0','合成崩溃')", signature); err != nil {
				t.Fatal(err)
			}
			s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "ios", Name: "iOS", Repo: "metasequoiaime/msime-ios"}}
			f := &fakeCrashGitHub{}
			const path = "/repos/metasequoiaime/msime-ios/issues"
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == path {
					f.mu.Lock()
					defer f.mu.Unlock()
					_ = json.NewEncoder(w).Encode(f.issues)
					return
				}
				f.ServeHTTP(w, r)
			})
			upstream := uncertainAdminWriteUpstream(t, handler, "POST", path, outcome)
			s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &githubapp.Cache{}}
			if response, err := s.adminGitHub.Get(t.Context(), "ghs_crash", path); err != nil || !response.OK() {
				t.Fatalf("预热缓存失败：%d %v", response.Status, err)
			}
			serveAdminJSON(t, s, releaseWrite("/api/crash-groups/"+signature+"/issue", "{}"), 502, nil)
			response, err := s.adminGitHub.Get(t.Context(), "ghs_crash", path)
			var issues []map[string]any
			if err != nil || response.Decode(&issues) != nil || len(issues) != 1 || !strings.Contains(issues[0]["title"].(string), "合成崩溃") {
				t.Fatalf("写入结果不确定时仍返回旧 issue 列表：%s %v", response.Body, err)
			}
		})
	}
}
