package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// 记录答案查找已有 PR 时必须查完分页，否则长期积累的开放 PR 会把同一问题挤到第二页而重复开 PR。
func TestQQOpenFAQPullSearchesAllPages(t *testing.T) {
	first := make([]map[string]any, 100)
	for i := range first {
		first[i] = map[string]any{"number": i + 1000, "title": "其他问题", "head": map[string]string{"ref": "qq-faq/other"}}
	}
	firstBody, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		second string
		status int
		want   int64
	}{
		{"found", `[{"number":77,"title":"docs(faq): add 「字体发虚怎么办」","head":{"ref":"qq-faq/existing"}}]`, 200, 77},
		{"not_found", `[]`, 200, 0},
		{"unavailable", `[]`, 502, 0},
		{"invalid", `not json`, 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			var mu sync.Mutex
			var pages []int
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/o/docs/pulls" {
					fake.ServeHTTP(w, r)
					return
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page == 0 {
					page = 1
				}
				mu.Lock()
				pages = append(pages, page)
				mu.Unlock()
				switch page {
				case 1:
					_, _ = w.Write(firstBody)
				case 2:
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.second)
				default:
					t.Errorf("不应请求第 %d 页", page)
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(upstream.Close)
			s.adminGitHub.APIURL = upstream.URL
			s.adminGitHub.HTTP = upstream.Client()
			got := s.qqOpenFAQPull(context.Background(), "test-token", "o/docs", "字体发虚怎么办")
			mu.Lock()
			gotPages := slices.Clone(pages)
			mu.Unlock()
			if got != tc.want {
				t.Errorf("返回 PR #%d，期望 #%d", got, tc.want)
			}
			if !slices.Equal(gotPages, []int{1, 2}) {
				t.Errorf("实际查询页码 %v，期望 [1 2]", gotPages)
			}
		})
	}
}
