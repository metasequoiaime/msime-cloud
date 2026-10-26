package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// 查重任意一页失败都不能视为没有重复项，否则重试可能创建第二个 FAQ PR。
func TestRecordQQAnswerStopsWhenPullSearchFails(t *testing.T) {
	for _, page := range []int{1, 2} {
		for _, test := range []struct {
			name   string
			status int
			body   string
		}{
			{"forbidden", http.StatusForbidden, `{}`},
			{"unavailable", http.StatusBadGateway, `{}`},
			{"invalid_json", http.StatusOK, `not json`},
			{"null", http.StatusOK, `null`},
			{"truncated_response", http.StatusOK, `[]`},
		} {
			t.Run(test.name+"/page_"+strconv.Itoa(page), func(t *testing.T) {
				s, fake := qqFAQRaceServer(t)
				locks := &faqRaceLocker{duplicateRaceLocker: newDuplicateRaceLocker(), secondAttempt: make(chan struct{})}
				s.qqLocks = locks
				first := make([]map[string]any, 100)
				for i := range first {
					first[i] = map[string]any{"number": i + 100, "title": "其他问题", "head": map[string]string{"ref": "qq-faq/other"}}
				}
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/repos/o/docs/pulls" {
						fake.ServeHTTP(w, r)
						return
					}
					if page == 2 && r.URL.Query().Get("page") == "1" {
						_ = json.NewEncoder(w).Encode(first)
						return
					}
					if test.name == "truncated_response" {
						w.Header().Set("Content-Length", "100")
					}
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, test.body)
				}))
				t.Cleanup(upstream.Close)
				s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
				var last string
				got := s.recordQQAnswer(context.Background(), "记录答案 鸿蒙版在哪下载", func(text string) { last = text })
				if got != 0 || last != qqReplyUnavailable {
					t.Errorf("查重失败仍报告创建成功：PR=%d，回复=%q", got, last)
				}
				if locks.releases.Load() != 1 {
					t.Error("查重失败后未释放跨副本锁")
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if len(fake.faqWrites) != 0 {
					t.Errorf("查重失败后仍发出 %d 次 GitHub 写请求", len(fake.faqWrites))
				}
			})
		}
	}
}
