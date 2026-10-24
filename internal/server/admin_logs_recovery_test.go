package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminLogsStreamReportsRecoveryAndPartialFailure(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			s, loki := adminLogsFixture(t)
			s.logPoll = 10 * time.Millisecond
			at := time.Now().Add(-time.Second)
			if partial {
				// 第一页读到五百行，前瞻行迫使同轮请求第二页。
				for i := range logStreamPageSize + 1 {
					loki.add(lokiEntry{ns: at.UnixNano() + int64(i), pod: "synthetic-pod", line: fmt.Sprintf("合成日志 %d", i)})
				}
			}
			release := make(chan struct{})
			var calls atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/loki/api/v1/query_range" {
					loki.serve(w, r)
					return
				}
				switch calls.Add(1) {
				case 1:
					w.WriteHeader(http.StatusServiceUnavailable)
				case 2:
					select {
					case <-release:
						loki.serve(w, r)
					case <-r.Context().Done():
					}
				case 3:
					if partial {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					<-r.Context().Done()
				default:
					<-r.Context().Done()
				}
			}))
			t.Cleanup(proxy.Close)
			s.config.Admin.Logs.LokiURL = proxy.URL
			s.loki = proxy.Client()
			cursor := at.Add(-time.Second).UnixNano()
			_, events, _ := openLogStream(t, s, "?cursor="+strconv.FormatInt(cursor, 10), nil)
			if event := nextEvent(t, events, "error"); event.data != `{"code":"logs_unavailable"}` {
				t.Fatal(event.data)
			}
			close(release)
			event := nextEvent(t, events, "lines")
			lines := streamLines(t, event)
			if !partial {
				if len(lines) != 0 || compareTS(event.id, strconv.FormatInt(cursor, 10)) <= 0 {
					t.Fatalf("空结果恢复未发送有效游标和空日志：%+v", event)
				}
				return
			}
			if len(lines) != logStreamPageSize {
				t.Fatalf("分页失败丢失了已读取的日志：%d", len(lines))
			}
			// 前端收到日志会清除故障状态，分页失败必须在日志之后重新通知。
			if event := nextEvent(t, events, "error"); event.data != `{"code":"logs_unavailable"}` {
				t.Fatal(event.data)
			}
		})
	}
}
