package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

type requestQueryCase struct {
	name, query, code string
	handle            func(*Server, http.ResponseWriter, *http.Request)
}

func requestQueryCases() []requestQueryCase {
	return []requestQueryCase{
		{"cloud", "text=nihao&scheme=pinyin&limit=0", "invalid_cloud_request", (*Server).cloud},
		{"logs", "pod=app-test-1&level=info&q=test&since=15m&limit=0", "invalid_limit", (*Server).adminLogs},
		{"stream", "pod=app-test-1&level=info&q=test&since=15m&backfill=0&cursor=bad", "invalid_cursor", (*Server).adminLogsStream},
	}
}

func requestQueryServer() *Server {
	s := &Server{}
	s.config.Cloud.URL = "https://cloud.example.test"
	s.config.Admin.Logs.LokiURL = "http://loki.example.test"
	return s
}

func requestWithQuery(query string) *http.Request {
	r := httptest.NewRequest("GET", "/?"+query, nil)
	return r.WithContext(account.WithAdminAccess(r.Context(), account.AdminAccess{Actor: "test", Permissions: []string{account.PermViewLogs}}))
}

func TestRequestQueryAllocationBudget(t *testing.T) {
	// 额外参数继续按现有接口忽略；放大重复解析的成本，给错误响应编码的缓存波动留出余量。
	var extra strings.Builder
	for i := range 64 {
		extra.WriteString("&extra" + strconv.Itoa(i) + "=value")
	}
	s := requestQueryServer()
	for _, tc := range requestQueryCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := requestWithQuery(tc.query + extra.String())
			var w *httptest.ResponseRecorder
			allocations := testing.AllocsPerRun(100, func() {
				w = httptest.NewRecorder()
				tc.handle(s, w, r)
			})
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("响应为 %d %s，期望 %s", w.Code, w.Body.String(), tc.code)
			}
			// 真实处理函数在请求校验末端返回，预算包含完整参数解析和错误响应。
			if allocations > 120 {
				t.Fatalf("请求校验分配 %.0f 次，预算 120 次", allocations)
			}
		})
	}
}

func TestRequestQueryValidationPriority(t *testing.T) {
	s := requestQueryServer()
	for _, tc := range []requestQueryCase{
		{"cloud_scheme", "text=%E6%B0%B4&scheme=bad&limit=0", "invalid_scheme", (*Server).cloud},
		{"cloud_text", "text=%E6%B0%B4&scheme=pinyin&limit=0", "pinyin_spelling_required", (*Server).cloud},
		{"logs_pod", "pod=Bad_Pod&since=bad&limit=0", "invalid_pod", (*Server).adminLogs},
		{"logs_level", "level=TRACE&since=bad&limit=0", "invalid_level", (*Server).adminLogs},
		{"logs_search", "q=%FF&since=bad&limit=0", "invalid_query", (*Server).adminLogs},
		{"logs_since", "since=bad&limit=0", "invalid_since", (*Server).adminLogs},
		{"stream_since", "since=bad&backfill=bad&cursor=bad", "invalid_since", (*Server).adminLogsStream},
		{"stream_backfill", "since=15m&backfill=bad&cursor=bad", "invalid_backfill", (*Server).adminLogsStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.handle(s, w, requestWithQuery(tc.query))
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("响应为 %d %s，期望 %s", w.Code, w.Body.String(), tc.code)
			}
		})
	}
}

func BenchmarkRequestQueryValidation(b *testing.B) {
	s := requestQueryServer()
	for _, tc := range requestQueryCases() {
		b.Run(tc.name, func(b *testing.B) {
			r := requestWithQuery(tc.query)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				tc.handle(s, w, r)
			}
		})
	}
}
