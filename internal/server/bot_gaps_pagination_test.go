package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// 汇总问题必须查完开放 Issue 的分页；后续页失败时不能把未找到误判为不存在。
func TestQQBotGapsIssuePagination(t *testing.T) {
	first := make([]json.RawMessage, 100)
	for i := range first {
		first[i] = json.RawMessage(fmt.Sprintf(`{"number":%d,"title":"其他问题"}`, i+1000))
	}
	first[0] = json.RawMessage(`{"number":90,"title":"QQ 机器人答不上的问题","performed_via_github_app":{"id":7}}`)
	first[1] = json.RawMessage(`{"number":91,"title":"QQ 机器人答不上的问题","pull_request":{"url":"x"},` + qqAppAuthor + `}`)
	fullPage, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, last  string
		status      int
		wantOK      bool
		wantCreated int
		wantNumber  int
	}{
		{"existing", `[{"number":77,"title":"QQ 机器人答不上的问题",` + qqAppAuthor + `}]`, 200, true, 0, 77},
		{"absent", `[]`, 200, true, 1, 123},
		{"unavailable", `[]`, 502, false, 0, 0},
		{"invalid", `not json`, 200, false, 0, 0},
		{"null", `null`, 200, false, 0, 0},
	} {
		for _, lastPage := range []int{1, 2} {
			t.Run(tc.name+"/page_"+strconv.Itoa(lastPage), func(t *testing.T) {
				s, fake, _ := qqServer(t)
				s.qq.config.GapsRepo = "o/docs"
				var mu sync.Mutex
				var pages []int
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" || r.URL.Path != "/repos/o/docs/issues" {
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
					switch {
					case page < lastPage:
						_, _ = w.Write(fullPage)
					case page == lastPage:
						w.WriteHeader(tc.status)
						_, _ = io.WriteString(w, tc.last)
					default:
						t.Errorf("不应请求第 %d 页", page)
						w.WriteHeader(500)
					}
				}))
				t.Cleanup(upstream.Close)
				s.adminGitHub.APIURL = upstream.URL
				s.adminGitHub.HTTP = upstream.Client()
				if ok := s.recordQQGap(context.Background(), "群友问：", "候选窗怎样调整"); ok != tc.wantOK {
					t.Errorf("记录结果为 %v，期望 %v", ok, tc.wantOK)
				}
				mu.Lock()
				gotPages := slices.Clone(pages)
				mu.Unlock()
				wantPages := []int{1}
				if lastPage == 2 {
					wantPages = append(wantPages, 2)
				}
				if !slices.Equal(gotPages, wantPages) {
					t.Errorf("实际查询页码 %v，期望 %v", gotPages, wantPages)
				}
				if len(fake.created) != tc.wantCreated {
					t.Errorf("新建了 %d 个 Issue，期望 %d", len(fake.created), tc.wantCreated)
				}
				if tc.wantOK {
					if fake.comments[fmt.Sprintf("/repos/o/docs/issues/%d/comments", tc.wantNumber)] == nil {
						t.Errorf("没有评论到预期的 Issue #%d：%v", tc.wantNumber, fake.comments)
					}
				} else if len(fake.comments) != 0 {
					t.Errorf("查询失败时不应发布评论：%v", fake.comments)
				}
			})
		}
	}
}
