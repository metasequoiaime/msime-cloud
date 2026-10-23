package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLogDigestStreamIdentity(t *testing.T) {
	s, loki := adminLogsFixture(t)
	s.logPoll = 5 * time.Millisecond
	s.logHeartbeat = time.Hour
	at := time.Now().Add(-time.Minute).UnixNano()
	prefix := "2026/10/01 14:47:01 INFO " + strings.Repeat("界🙂", maxLogMessageBytes)
	entries := []lokiEntry{
		{ns: at, pod: "synthetic-a", line: prefix + "first"},
		{ns: at, pod: "synthetic-a", line: prefix + "second"},
		{ns: at, pod: "synthetic-b", line: prefix + "first"},
		{ns: at + 1, pod: "synthetic-a", line: "2026-10-01T14:47:01.378828677Z stderr F 2026/10/01 14:47:01 WARN 中文日志\"\\\r\n\t<&>"},
		{ns: at + 2, pod: "synthetic-a", line: ""},
	}
	check := func(got []logLineJSON, raw []lokiEntry) {
		t.Helper()
		sortLogEntries(raw)
		if len(got) != len(raw) {
			t.Fatalf("收到 %d 行，期望 %d 行", len(got), len(raw))
		}
		ids := map[string]bool{}
		for i, entry := range raw {
			want := entry.JSON()
			want.ID = strconv.FormatInt(entry.ns, 10) + ":" + strconv.FormatUint(previousLogKey(entry).hash, 16)
			if got[i] != want || ids[got[i].ID] {
				t.Fatalf("日志字段、完整原文标识或排序发生变化：第 %d 行", i)
			}
			ids[got[i].ID] = true
		}
	}
	loki.add(entries...)
	_, events, cancel := openLogStream(t, s, "?backfill=20", nil)
	defer cancel()
	check(streamLines(t, nextEvent(t, events, "lines")), slices.Clone(entries))
	for i := range entries {
		entries[i].ns = time.Now().UnixNano() + int64(i)
	}
	loki.add(entries...)
	check(streamLines(t, nextEvent(t, events, "lines")), entries)
}

func TestLogDigestJSONLengthScaling(t *testing.T) {
	measure := func(size int) testing.BenchmarkResult {
		s, loki := adminLogsFixture(t)
		at := time.Now().Add(-time.Second).UnixNano()
		raw := lokiEntry{ns: at, pod: "synthetic-pod", line: "2026-10-01T14:47:01.378828677Z stderr F " + strings.Repeat("x", size)}
		loki.add(raw)
		tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: at - 1, floor: at - 1}
		entries, gaps, err := s.pollLogs(t.Context(), tail, at+1)
		if err != nil || len(entries) != 1 || len(gaps) != 0 {
			t.Fatal("轮询未取得待转换的完整日志", err, len(entries), gaps)
		}
		var got []logLineJSON
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got = logLinesJSON(entries)
			}
		})
		if len(got) != 1 || got[0] != raw.JSON() || len(got[0].Message) != maxLogMessageBytes {
			t.Fatal("复用摘要改变了完整原文标识或截断正文")
		}
		return result
	}
	short := measure(maxLogMessageBytes)
	long := measure(1 << 20)
	t.Logf("4 KiB 正文：%s；1 MiB 正文：%s", short, long)
	// 固定 CRI 前缀排除前缀解析的扫描差异；输出都只有 4 KiB 正文，预算留出调度余量。
	if long.NsPerOp() > short.NsPerOp()*16 {
		t.Fatalf("轮询后的 JSON 转换仍随原文长度增长：短行 %d ns/op，长行 %d ns/op", short.NsPerOp(), long.NsPerOp())
	}
}

func BenchmarkLogDigestPollFrame(b *testing.B) {
	for _, tc := range []struct{ count, width int }{{1, 64}, {200, 64}, {200, 4096}, {1, 1 << 20}} {
		b.Run(fmt.Sprintf("%dx%d", tc.count, tc.width), func(b *testing.B) {
			at := time.Now().Add(-time.Second).UnixNano()
			values := make([][2]string, tc.count)
			message := strings.Repeat("x", tc.width)
			for i := range values {
				values[i] = [2]string{strconv.FormatInt(at+int64(i), 10), message}
			}
			response, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"stream": map[string]string{"pod": "synthetic-pod"}, "values": values}}}})
			if err != nil {
				b.Fatal(err)
			}
			s := &Server{loki: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/loki/api/v1/query_range" || r.URL.Query().Get("direction") != "forward" {
					return nil, fmt.Errorf("轮询请求发生变化")
				}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(response)), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response))}, nil
			})}}
			s.config.Admin.Logs.LokiURL = "https://loki.example.test"
			b.ReportAllocs()
			b.SetBytes(int64(tc.count * tc.width))
			for b.Loop() {
				tail := &logTail{query: "{}", seen: map[logKey]struct{}{}, cursor: at - 1, floor: at - 1}
				entries, gaps, err := s.pollLogs(context.Background(), tail, at+int64(time.Second))
				if err != nil || len(gaps) != 0 || len(entries) != tc.count {
					b.Fatal("轮询未完整读出日志", err, len(entries), gaps)
				}
				body, err := json.Marshal(map[string]any{"lines": logLinesJSON(entries)})
				if err != nil {
					b.Fatal(err)
				}
				frame := logStreamFrame("lines", strconv.FormatInt(tail.cursor, 10), body)
				if _, err := io.Discard.Write(frame); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
