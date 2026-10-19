package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAdminLogsStreamRetriesInitialBackfillBeforeLivePolling(t *testing.T) {
	for _, failures := range []int{1, 2} {
		t.Run(strconv.Itoa(failures), func(t *testing.T) {
			s, loki := adminLogsFixture(t)
			s.logPoll = 10 * time.Millisecond
			at := time.Now()
			loki.add(
				logEntryAt(at.Add(-3*time.Minute), "synthetic-pod", "INFO", "不在回填额度内"),
				logEntryAt(at.Add(-2*time.Minute), "synthetic-pod", "INFO", "回填第一行"),
				logEntryAt(at.Add(-time.Minute), "synthetic-pod", "INFO", "回填第二行"),
			)
			var mu sync.Mutex
			var backfills []url.Values
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("direction") == "backward" {
					mu.Lock()
					backfills = append(backfills, r.URL.Query())
					attempt := len(backfills)
					mu.Unlock()
					if attempt == 1 {
						// 新行位于初次回填终点之后，恢复后必须先补回历史，再开始实时查询。
						end, err := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
						if err != nil {
							t.Error(err)
						}
						loki.add(lokiEntry{ns: end + 1, pod: "synthetic-pod", line: "恢复后的新日志"})
					}
					if attempt <= failures {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				loki.serve(w, r)
			}))
			t.Cleanup(proxy.Close)
			s.config.Admin.Logs.LokiURL = proxy.URL
			s.loki = proxy.Client()
			_, events, _ := openLogStream(t, s, "?backfill=2", nil)
			if ev := nextEvent(t, events, "error"); ev.data != `{"code":"logs_unavailable"}` {
				t.Fatal(ev.data)
			}
			got := map[string]int{}
			for got["恢复后的新日志"] == 0 {
				for _, line := range streamLines(t, nextEvent(t, events, "lines")) {
					got[line.Message]++
					if line.Message == "恢复后的新日志" && (got["回填第一行"] != 1 || got["回填第二行"] != 1) {
						t.Fatalf("来源恢复后直接进入实时查询，初次回填永久丢失：%v", got)
					}
				}
			}
			if len(got) != 3 || got["恢复后的新日志"] != 1 {
				t.Fatalf("恢复回填返回了额外或重复日志：%v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(backfills) != failures+1 {
				t.Fatalf("回填尝试次数错误：%d", len(backfills))
			}
			for _, query := range backfills[1:] {
				if query.Encode() != backfills[0].Encode() {
					t.Fatal("重试改变了初次回填的窗口或条数", backfills)
				}
			}
		})
	}
}

func TestAdminLogsStreamBackfillRetryKeepsFailureLimitAndHeartbeat(t *testing.T) {
	s, loki := adminLogsFixture(t)
	s.logPoll = 10 * time.Millisecond
	s.logHeartbeat = time.Nanosecond
	var mu sync.Mutex
	var directions []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/query_range" {
			mu.Lock()
			directions = append(directions, r.URL.Query().Get("direction"))
			mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		loki.serve(w, r)
	}))
	t.Cleanup(proxy.Close)
	s.config.Admin.Logs.LokiURL = proxy.URL
	s.loki = proxy.Client()
	_, events, _ := openLogStream(t, s, "?backfill=2", nil)
	errors, heartbeats := 0, 0
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				mu.Lock()
				defer mu.Unlock()
				if len(directions) != logStreamMaxFailures || errors != 1 || heartbeats == 0 {
					t.Fatalf("回填失败未保持重试上限、单次错误通知或心跳：directions=%v errors=%d heartbeats=%d", directions, errors, heartbeats)
				}
				for _, direction := range directions {
					if direction != "backward" {
						t.Fatal("回填失败时仍进入了实时查询", directions)
					}
				}
				return
			}
			if ev.event == "error" {
				errors++
			}
			if ev.comment {
				heartbeats++
			}
			if ev.event == "lines" || ev.id != "" {
				t.Fatal("回填失败时确认了尚未读取的游标", ev)
			}
		case <-timeout:
			t.Fatal("持续回填失败后没有关闭连接")
		}
	}
}
