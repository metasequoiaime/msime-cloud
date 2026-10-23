package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestPollLogsSameTimestampDoesNotSilentlySkip(t *testing.T) {
	for _, total := range []int{499, 500, 505, 1000, 1500, 2000, 2005} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			s, loki := adminLogsFixture(t)
			at := time.Now().Add(-10 * time.Second).UnixNano()
			for i := range total {
				loki.add(lokiEntry{ns: at, pod: fmt.Sprintf("p-%d", i%3), line: fmt.Sprintf("line-%04d", i)})
			}
			tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: at - 1, floor: at - 1}
			now := at + int64(time.Second)
			seen := map[logKey]bool{}
			hadGap := false
			for range 6 {
				before := len(loki.seen())
				entries, gap, err := s.pollLogs(context.Background(), tail, now)
				if err != nil {
					t.Fatal(err)
				}
				hadGap = hadGap || len(gap) > 0
				for _, missing := range gap {
					if missing.from != at || missing.to != at+1 {
						t.Fatal("incorrect saturated timestamp gap", missing, at)
					}
				}
				if len(entries) > logStreamPageSize*logStreamPages || len(loki.seen())-before > logStreamPages {
					t.Fatal("poll exceeded its output or request limit", len(entries), len(loki.seen())-before)
				}
				for _, entry := range entries {
					if seen[entry.key()] {
						t.Fatal("duplicate log", entry)
					}
					seen[entry.key()] = true
				}
				if !tail.busy {
					break
				}
			}
			if got, want := len(seen), min(total, logStreamMaxBatchSize); got != want {
				t.Fatalf("delivered %d/%d logs; want %d, gap=%v", got, total, want, hadGap)
			}
			if hadGap != (total > logStreamMaxBatchSize) {
				t.Fatalf("gap=%v for %d logs", hadGap, total)
			}
			if tail.busy || tail.cursor != now {
				t.Fatal("poll did not finish", tail.busy, tail.cursor, now)
			}
			for _, query := range loki.seen() {
				limit, err := strconv.Atoi(query.Get("limit"))
				if err != nil || limit > logStreamMaxBatchSize+1 {
					t.Fatal("unbounded query", query)
				}
			}
		})
	}
}

func TestPollLogsSaturatedTimestampKeepsOtherLateLogs(t *testing.T) {
	s, loki := adminLogsFixture(t)
	at := time.Now().Add(-10 * time.Second).UnixNano()
	for i := range logStreamMaxBatchSize + 5 {
		loki.add(lokiEntry{ns: at, pod: "p", line: fmt.Sprintf("line-%04d", i)})
	}
	floor := at - 10
	tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: floor, floor: floor}
	now := at + int64(time.Second)
	gapCount := 0
	for range 4 {
		_, gaps, err := s.pollLogs(context.Background(), tail, now)
		if err != nil {
			t.Fatal(err)
		}
		gapCount += len(gaps)
		if !tail.busy {
			break
		}
	}
	if gapCount != 1 || tail.floor != floor {
		t.Fatal("gap must only skip the saturated timestamp", gapCount, tail.floor, floor)
	}
	loki.add(lokiEntry{ns: at - 1, pod: "p", line: "late before saturated timestamp"})
	entries, gaps, err := s.pollLogs(context.Background(), tail, now+1)
	if err != nil || len(gaps) != 0 || len(entries) != 1 || entries[0].line != "late before saturated timestamp" {
		t.Fatal("late log lost or gap repeated", entries, gaps, err)
	}
	tail.cursor += int64(logStreamOverlap) * 2
	if _, _, err := s.pollLogs(context.Background(), tail, tail.cursor+1); err != nil || len(tail.readAt) != 0 {
		t.Fatal("expired pagination state was retained", tail.readAt, err)
	}
}

func TestPollLogsSameTimestampResumeDoesNotLoseRemainingLines(t *testing.T) {
	s, loki := adminLogsFixture(t)
	at := time.Now().Add(-10 * time.Second).UnixNano()
	for i := range 1500 {
		loki.add(lokiEntry{ns: at, pod: "p", line: fmt.Sprintf("line-%04d", i)})
	}
	tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: at - 1, floor: at - 1}
	now := at + int64(time.Second)
	entries, gap, err := s.pollLogs(context.Background(), tail, now)
	if err != nil || len(gap) > 0 || !tail.busy || len(entries) != 1000 || tail.cursor >= at {
		t.Fatal("unfinished timestamp was acknowledged", len(entries), gap, err, tail.busy, tail.cursor, at)
	}
	seen := map[logKey]bool{}
	for _, entry := range entries {
		seen[entry.key()] = true
	}
	// 重连会重发尚未读完的时间戳，允许客户端去重，但不能跳过剩余行。
	resumed := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: tail.cursor, floor: tail.cursor}
	for range 3 {
		entries, gap, err = s.pollLogs(context.Background(), resumed, now)
		if err != nil || len(gap) > 0 {
			t.Fatal(gap, err)
		}
		for _, entry := range entries {
			seen[entry.key()] = true
		}
		if !resumed.busy {
			break
		}
	}
	if len(seen) != 1500 {
		t.Fatal("resume lost logs", len(seen))
	}
}

func TestPollLogsSameTimestampKeepsProgressAfterPageFailure(t *testing.T) {
	s, loki := adminLogsFixture(t)
	at := time.Now().Add(-10 * time.Second).UnixNano()
	for i := range 1500 {
		loki.add(lokiEntry{ns: at, pod: "p", line: fmt.Sprintf("line-%04d", i)})
	}
	requests := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		loki.serve(w, r)
	}))
	defer proxy.Close()
	s.config.Admin.Logs.LokiURL = proxy.URL
	s.loki = proxy.Client()
	tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: at - 1, floor: at - 1}
	now := at + int64(time.Second)
	first, _, err := s.pollLogs(context.Background(), tail, now)
	if err == nil || len(first) != 500 || !tail.busy || tail.cursor >= at {
		t.Fatal("partial page failure lost progress", len(first), err, tail.busy, tail.cursor)
	}
	remaining, gap, err := s.pollLogs(context.Background(), tail, now)
	if err != nil || len(gap) > 0 || len(first)+len(remaining) != 1500 || tail.busy {
		t.Fatal(len(first), len(remaining), gap, err, tail.busy)
	}
}

func TestPollLogsSameTimestampCountsRawDuplicates(t *testing.T) {
	s, loki := adminLogsFixture(t)
	at := time.Now().Add(-10 * time.Second).UnixNano()
	for range 1000 {
		loki.add(lokiEntry{ns: at, pod: "p", line: "duplicate"})
	}
	loki.add(lokiEntry{ns: at, pod: "p", line: "unique after duplicates"})
	tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: at - 1, floor: at - 1}
	var all []logStreamEntry
	for range 4 {
		entries, gaps, err := s.pollLogs(context.Background(), tail, at+int64(time.Second))
		if err != nil || len(gaps) != 0 {
			t.Fatal(gaps, err)
		}
		all = append(all, entries...)
		if !tail.busy {
			break
		}
	}
	if tail.busy || len(all) != 2 || all[1].line != "unique after duplicates" {
		t.Fatal("raw duplicates blocked pagination", tail.busy, all)
	}
}

func TestAdminLogsStreamSameTimestampPaging(t *testing.T) {
	for _, total := range []int{1500, 2005} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			s, loki := adminLogsFixture(t)
			s.logPoll = 20 * time.Millisecond
			at := time.Now().Add(-10 * time.Second).UnixNano()
			for i := range total {
				loki.add(lokiEntry{ns: at, pod: fmt.Sprintf("p-%d", i%3), line: fmt.Sprintf("line-%04d", i)})
			}
			loki.add(lokiEntry{ns: at + int64(time.Second), pod: "p", line: "after timestamp"})
			_, events, _ := openLogStream(t, s, "?cursor="+strconv.FormatInt(at-1, 10), nil)
			seen := map[string]bool{}
			gapCount := 0
			deadline := time.After(5 * time.Second)
			for !seen["after timestamp"] {
				select {
				case event, ok := <-events:
					if !ok {
						t.Fatal("stream closed before later logs arrived")
					}
					switch event.event {
					case "lines":
						lines := streamLines(t, event)
						if len(lines) > logStreamPageSize*logStreamPages {
							t.Fatal("SSE frame exceeded the output limit", len(lines))
						}
						for _, line := range lines {
							if seen[line.Message] {
								t.Fatal("duplicate SSE log", line)
							}
							seen[line.Message] = true
						}
						if len(seen) == 1000 && event.id != strconv.FormatInt(at-1, 10) {
							t.Fatal("SSE cursor skipped unfinished timestamp", event.id, at)
						}
					case "gap":
						gapCount++
						var gap map[string]string
						if err := json.Unmarshal([]byte(event.data), &gap); err != nil || gap["from"] != strconv.FormatInt(at, 10) || gap["to"] != strconv.FormatInt(at+1, 10) {
							t.Fatal("SSE gap has the wrong range", event, err)
						}
					case "error", "end":
						t.Fatal(event)
					}
				case <-deadline:
					t.Fatal("stream stalled", len(seen), gapCount)
				}
			}
			expectedGaps := 0
			if total > logStreamMaxBatchSize {
				expectedGaps = 1
			}
			if len(seen) != min(total, logStreamMaxBatchSize)+1 || gapCount != expectedGaps {
				t.Fatal("logs were lost without the correct gap", len(seen), total, gapCount)
			}
		})
	}
}
