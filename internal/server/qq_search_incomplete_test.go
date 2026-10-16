package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func qqIncompleteSearchServer(t *testing.T, items string) (*Server, *qqFake, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	s, fake, _ := qqServer(t)
	var incomplete atomic.Bool
	incomplete.Store(true)
	var searches atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			searches.Add(1)
			_, _ = fmt.Fprintf(w, `{"total_count":7,"incomplete_results":%t,"items":%s}`, incomplete.Load(), items)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
	return s, fake, &incomplete, &searches
}

// 不完整的查重结果不能证明没有重复项，也不能作为补充到已有 issue 的依据。
func TestQQReportStopsOnIncompleteSearch(t *testing.T) {
	for _, items := range []string{`[]`, `[{"number":7,"title":"候选框透明度","body":"希望支持透明度"}]`} {
		t.Run(items, func(t *testing.T) {
			s, fake, _, _ := qqIncompleteSearchServer(t, items)
			fake.duplicate = `{"duplicate":7}`
			report := qqReport{Title: "候选框透明度", Proposal: "希望支持透明度", Keywords: "候选框 透明度"}
			if _, _, err := s.fileQQReport(context.Background(), "o/r", "", report, nil, 0, qqNotes{}); err == nil {
				t.Error("不完整的查重仍然报告提交成功")
			}
			if len(fake.created) != 0 || len(fake.comments) != 0 {
				t.Errorf("不完整的查重后仍写入：新建 %d 个 issue，发布 %d 条评论", len(fake.created), len(fake.comments))
			}
		})
	}
}

// 不完整的答疑搜索应报告失败，不能继续退词重搜或声称没有相关反馈。
func TestQQIssueSearchFailsOnIncompleteResults(t *testing.T) {
	for _, items := range []string{`[]`, `[{"number":7,"title":"候选框透明度"}]`} {
		t.Run(items, func(t *testing.T) {
			s, _, _, searches := qqIncompleteSearchServer(t, items)
			hits, failed := s.qqSearchIssueHitsIn(context.Background(), []string{"o/r"}, "候选框 透明度 设置", false)
			if !failed || len(hits) != 0 {
				t.Errorf("不完整响应被视为成功：failed=%v，hits=%v", failed, hits)
			}
			if got := searches.Load(); got != 1 {
				t.Errorf("不完整响应后仍退词重搜：请求数=%d", got)
			}
		})
	}
}

// 不完整的数量不能对外展示或写入统计缓存；后续完整响应仍能恢复统计。
func TestQQRepoStatsRejectsIncompleteResults(t *testing.T) {
	s, _, incomplete, _ := qqIncompleteSearchServer(t, `[{"number":7}]`)
	s.config.Admin.GitHub.Platforms = nil
	if stats := s.qqRepoStats(context.Background()); stats != "" {
		t.Errorf("展示了不完整的统计：%q", stats)
	}
	s.qq.mu.Lock()
	cached := s.qq.stats
	s.qq.mu.Unlock()
	if cached != "" {
		t.Errorf("缓存了不完整的统计：%q", cached)
	}
	// 清空传输层短缓存，让下次访问上游；统计缓存不能保留半成品。
	s.adminGitHub.Invalidate("/search/issues")
	incomplete.Store(false)
	if stats := s.qqRepoStats(context.Background()); !strings.Contains(stats, "现在开着的 issue 7 个") {
		t.Fatalf("完整响应未恢复正常统计：%q", stats)
	}
}
