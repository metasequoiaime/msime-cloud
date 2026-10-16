package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func qqSearchResponseServer(t *testing.T, body string) (*Server, *qqFake, *atomic.Int32) {
	t.Helper()
	s, fake, _ := qqServer(t)
	var searches atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			searches.Add(1)
			_, _ = io.WriteString(w, body)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	s.adminGitHub.APIURL, s.adminGitHub.HTTP = upstream.URL, upstream.Client()
	return s, fake, &searches
}

// 查重无法确认结果时必须停止发布，只有有效空列表才表示没有重复项。
func TestQQReportRejectsSearchResponsesWithoutItems(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"null", `null`, false},
		{"missing_items", `{}`, false},
		{"null_items", `{"items":null}`, false},
		{"object_items", `{"items":{}}`, false},
		{"empty_items", `{"items":[]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, fake, _ := qqSearchResponseServer(t, test.body)
			report := qqReport{Title: "候选框透明度", Proposal: "希望支持透明度", Keywords: "候选框 透明度"}
			_, _, err := s.fileQQReport(context.Background(), "o/r", "", report, nil, 0, qqNotes{})
			if (err == nil) != test.valid {
				t.Errorf("有效性=%v，发布结果 err=%v", test.valid, err)
			}
			wantCreated := 0
			if test.valid {
				wantCreated = 1
			}
			if len(fake.created) != wantCreated || len(fake.comments) != 0 {
				t.Errorf("查重后的写入不符合预期：created=%v，comments=%v", fake.created, fake.comments)
			}
		})
	}
}

// 搜索响应缺少有效 items 时不能退词重搜，也不能把失败报告为没有反馈。
func TestQQIssueSearchRejectsResponsesWithoutItems(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"items":null}`, `{"items":{}}`} {
		t.Run(body, func(t *testing.T) {
			s, _, searches := qqSearchResponseServer(t, body)
			hits, failed := s.qqSearchIssueHitsIn(context.Background(), []string{"o/r"}, "候选框 透明度 设置", false)
			if !failed || len(hits) != 0 {
				t.Errorf("无效响应被视为有效搜索：failed=%v，hits=%v", failed, hits)
			}
			if got := searches.Load(); got != 1 {
				t.Errorf("无效响应后仍退词重搜：请求数=%d", got)
			}
		})
	}
}
