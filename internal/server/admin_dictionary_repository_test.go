package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestDictionaryReadRepositoryFailures(t *testing.T) {
	for _, endpoint := range []string{"/api/dictionary", "/api/dictionary/file?path=custom/words.txt"} {
		for _, tc := range []struct {
			name, metadata, code              string
			tokenStatus, metadataStatus, want int
		}{
			{"令牌被拒绝", "", "github_rejected", 403, 0, 502},
			{"令牌不可用", "", "github_unavailable", 503, 0, 503},
			{"仓库不存在", "", "not_found", 201, 404, 404},
			{"仓库访问被拒绝", "", "github_error", 201, 403, 502},
			{"仓库不可用", "", "github_unavailable", 201, 503, 503},
			{"损坏的元数据", `{"private":"upstream detail"`, "github_unavailable", 201, 200, 503},
			{"缺少默认分支", `{"description":"private upstream detail"}`, "github_unavailable", 201, 200, 503},
			{"错误的默认分支类型", `{"default_branch":123}`, "github_unavailable", 201, 200, 503},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var paths []string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					paths = append(paths, r.URL.Path)
					mu.Unlock()
					if strings.HasSuffix(r.URL.Path, "/access_tokens") {
						w.WriteHeader(tc.tokenStatus)
						_, _ = w.Write([]byte(`{"token":"synthetic-token","message":"private upstream detail"}`))
						return
					}
					if r.URL.Path != "/repos/"+dictTestRepo {
						t.Errorf("失败后继续请求了上游：%s", r.URL.Path)
					}
					w.WriteHeader(tc.metadataStatus)
					_, _ = w.Write([]byte(tc.metadata))
				}))
				t.Cleanup(upstream.Close)
				client := &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client()}
				s := &Server{adminGitHub: client, config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
				w := dictionaryDirect(s, "GET", endpoint, "", false)
				if w.Code != tc.want || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "synthetic-token") {
					t.Fatalf("错误响应不符：%d %s", w.Code, w.Body.String())
				}
				wantRequests := 2
				if tc.tokenStatus != 201 {
					wantRequests = 1
				}
				mu.Lock()
				defer mu.Unlock()
				if len(paths) != wantRequests {
					t.Fatalf("上游请求顺序不符：%v", paths)
				}
			})
		}
	}
}

func TestDictionaryReadRepositoryMetadataFields(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     int
	}{
		{"/api/dictionary", 503},
		{"/api/dictionary/file?path=custom/words.txt", 200},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			f := &dictionaryMock{text: "甲\tjia\t1\n"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/"+dictTestRepo {
					_, _ = w.Write([]byte(`{"default_branch":"main","description":123}`))
					return
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(upstream.Close)
			client := &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: upstream.URL, HTTP: upstream.Client()}
			s := &Server{adminGitHub: client, config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
			w := dictionaryDirect(s, "GET", tc.endpoint, "", false)
			if w.Code != tc.want {
				t.Fatalf("各入口的元数据字段解码发生变化：%d %s", w.Code, w.Body.String())
			}
		})
	}
}
