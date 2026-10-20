package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestDictionaryTreeFilteringAllocationBudget(t *testing.T) {
	f := &dictionaryMock{text: "甲\tjia\t1\n"}
	sha := dictionaryBlobSHA([]byte(f.text))
	for i := range 5000 {
		f.tree = append(f.tree, dictionaryTreeItem{Path: fmt.Sprintf("assets/synthetic-%04d.png", i), Type: "blob", SHA: sha, Size: 10})
	}
	for i := range 8 {
		f.tree = append(f.tree, dictionaryTreeItem{Path: fmt.Sprintf("sources/demo/word-%02d.txt", 7-i), Type: "blob", SHA: sha, Size: int64(len(f.text))})
	}
	client := dictionaryMockClient(t, f)
	client.Cache = &githubapp.Cache{}
	s := &Server{adminGitHub: client, config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
	for _, endpoint := range []string{"tree", "search"} {
		t.Run(endpoint, func(t *testing.T) {
			path := "/api/dictionary"
			if endpoint == "search" {
				path += "/search?q=甲&scope=sources"
			}
			r := httptest.NewRequest("GET", path, nil)
			handle := s.dictionaryTree
			if endpoint == "search" {
				handle = s.dictionarySearch
			}
			var w *httptest.ResponseRecorder
			run := func(tb testing.TB) {
				w = httptest.NewRecorder()
				handle(w, r, client)
				if w.Code != 200 {
					tb.Fatalf("词库读取失败：%d %s", w.Code, w.Body.String())
				}
			}
			run(t)
			wantBody := w.Body.String()
			if endpoint == "tree" {
				listing := decodeDict[struct {
					Files  []dictionaryTreeItem `json:"files"`
					Counts map[string]int       `json:"counts"`
				}](t, w)
				if len(listing.Files) != 8 || listing.Counts["sources"] != 8 {
					t.Fatalf("目录筛选或计数改变：%+v", listing)
				}
				for i, file := range listing.Files {
					if want := fmt.Sprintf("sources/demo/word-%02d.txt", i); file.Path != want {
						t.Fatalf("目录排序改变：%s，期望 %s", file.Path, want)
					}
				}
			} else {
				search := decodeDict[dictionarySearchResponse](t, w)
				if search.TotalFiles != 8 || search.NextCursor != 4 || !search.HasMore || len(search.Files) != 4 {
					t.Fatalf("搜索筛选或分页改变：%+v", search)
				}
				for i, file := range search.Files {
					if want := fmt.Sprintf("sources/demo/word-%02d.txt", i); file.Path != want || file.Total != 1 {
						t.Fatalf("搜索顺序或内容改变：%+v，期望 %s", file, want)
					}
				}
			}
			response, err := client.Get(t.Context(), "synthetic-token", "/repos/"+dictTestRepo+"/git/trees/main?recursive=1")
			if err != nil || !response.OK() {
				t.Fatalf("读取已缓存的树失败：%v", err)
			}
			decode := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					var tree struct {
						SHA       string               `json:"sha"`
						Tree      []dictionaryTreeItem `json:"tree"`
						Truncated bool                 `json:"truncated"`
					}
					if err := response.Decode(&tree); err != nil || len(tree.Tree) != len(f.tree) {
						b.Fatalf("解码基线失败：%v", err)
					}
				}
			})
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					run(b)
				}
			})
			if w.Body.String() != wantBody {
				t.Fatal("重复读取改变了缓存中的树或响应")
			}
			t.Logf("解码基线：%s；真实处理函数：%s", decode.MemString(), result.MemString())
			// 扣除必需的树解码，只给请求、筛选和响应保留余量，排除第二份整树大小的数组。
			if extra := result.AllocedBytesPerOp() - decode.AllocedBytesPerOp(); extra > 128<<10 {
				t.Fatalf("筛选和响应额外分配 %d 字节，超过 128 KiB 预算", extra)
			}
		})
	}
}

func TestDictionaryTreeEmptyFilesRemainJSONArray(t *testing.T) {
	for _, tree := range []string{"null", "[]", `[{"path":"README.md","type":"blob"}]`} {
		t.Run(tree, func(t *testing.T) {
			f := &dictionaryMock{}
			if err := json.Unmarshal([]byte(tree), &f.tree); err != nil {
				t.Fatal(err)
			}
			// 原有模拟服务把 nil 解释为默认树，因此用 HTTP 包装器明确返回 null。
			client := dictionaryMockClient(t, f)
			client.HTTP.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/git/trees/") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"tree":` + tree + `}`)), Header: http.Header{}}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			s := &Server{adminGitHub: client, config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: dictTestRepo}}}}
			w := dictionaryDirect(s, "GET", "/api/dictionary", "", false)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"files":[]`) {
				t.Fatalf("空词库文件列表改变：%d %s", w.Code, w.Body.String())
			}
		})
	}
}
