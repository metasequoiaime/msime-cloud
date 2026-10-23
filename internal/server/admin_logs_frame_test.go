package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

type logFrameWriter struct {
	header                         http.Header
	status, writes, flushes, bytes int
	readDeadlines, writeDeadlines  int
	failWrite, failFlush           int
	check                          bool
	lines                          string
	cancel                         context.CancelFunc
}

func (w *logFrameWriter) Header() http.Header    { return w.header }
func (w *logFrameWriter) WriteHeader(status int) { w.status = status }
func (w *logFrameWriter) SetReadDeadline(at time.Time) error {
	if !at.IsZero() {
		return errors.New("读取期限没有清除")
	}
	w.readDeadlines++
	return nil
}
func (w *logFrameWriter) SetWriteDeadline(at time.Time) error {
	if at.IsZero() {
		return errors.New("写出期限没有设置")
	}
	w.writeDeadlines++
	return nil
}
func (w *logFrameWriter) Write(data []byte) (int, error) {
	if w.check {
		return w.WriteString(string(data))
	}
	return w.consume(len(data))
}
func (w *logFrameWriter) WriteString(frame string) (int, error) {
	if w.check {
		want := "retry: 5000\n\n"
		switch w.writes {
		case 1:
			id, _, ok := strings.Cut(strings.TrimPrefix(frame, "id: "), "\n")
			if _, err := strconv.ParseInt(id, 10, 64); !ok || err != nil {
				return 0, errors.New("回填游标格式发生变化")
			}
			want = "id: " + id + "\nevent: lines\ndata: " + w.lines + "\n\n"
		case 2:
			want = "event: pods\ndata: {\"pods\":[\"synthetic-pod\"]}\n\n"
		}
		if frame != want {
			return 0, errors.New("SSE 事件顺序或完整字节发生变化")
		}
	}
	return w.consume(len(frame))
}
func (w *logFrameWriter) consume(n int) (int, error) {
	w.writes++
	if w.writes == w.failWrite {
		return 0, errors.New("合成写出失败")
	}
	w.bytes += n
	return n, nil
}
func (w *logFrameWriter) FlushError() error {
	w.flushes++
	if w.flushes == w.failFlush {
		return errors.New("合成刷新失败")
	}
	if w.flushes == 3 {
		w.cancel()
	}
	return nil
}

type logFrameFixture struct {
	server                 *Server
	writer                 logFrameWriter
	count, queries, closed int
}

type logFrameResponseBody struct {
	io.Reader
	closed *int
}

func (r *logFrameResponseBody) Close() error { *r.closed++; return nil }

func newLogFrameFixture(tb testing.TB, count, width int) *logFrameFixture {
	tb.Helper()
	return newLogFrameMessageFixture(tb, count, strings.Repeat("x", width))
}

func newLogFrameMessageFixture(tb testing.TB, count int, message string) *logFrameFixture {
	tb.Helper()
	values := make([][2]string, count)
	entries := make([]lokiEntry, count)
	at := time.Now().Add(-time.Minute).UnixNano()
	for i := range entries {
		entries[i] = lokiEntry{ns: at + int64(i), pod: "synthetic-pod", line: message}
		values[i] = [2]string{strconv.FormatInt(entries[i].ns, 10), entries[i].line}
	}
	response, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"stream": map[string]string{"pod": "synthetic-pod"}, "values": values}}}})
	if err != nil {
		tb.Fatal(err)
	}
	lines, err := json.Marshal(map[string]any{"lines": logLinesJSON(entries)})
	if err != nil {
		tb.Fatal(err)
	}
	fixture := &logFrameFixture{count: count, writer: logFrameWriter{check: true, lines: string(lines)}}
	fixture.server = &Server{lifetime: context.Background(), logStreamsDone: make(chan struct{}), logPoll: time.Hour, logHeartbeat: time.Hour, logMaxDuration: time.Hour}
	fixture.server.config.Admin.Logs = AdminLogsConfig{LokiURL: "https://loki.example.test", Selector: defaultLogSelector}
	fixture.server.loki = &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "loki.example.test" {
			return nil, errors.New("日志查询方法或目标发生变化")
		}
		body := response
		if r.URL.Path == "/loki/api/v1/query" {
			body = []byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pod":"synthetic-pod"}}]}}`)
		} else if r.URL.Path != "/loki/api/v1/query_range" || r.URL.Query().Get("limit") != strconv.Itoa(count) || r.URL.Query().Get("direction") != "backward" {
			return nil, errors.New("回填查询方法或限制发生变化")
		}
		fixture.queries++
		return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Header: make(http.Header), Body: &logFrameResponseBody{Reader: bytes.NewReader(body), closed: &fixture.closed}}, nil
	})}
	return fixture
}

func (f *logFrameFixture) run(tb testing.TB) {
	tb.Helper()
	ctx, cancel := context.WithCancel(tb.Context())
	defer cancel()
	ctx = account.WithAdminAccess(ctx, account.AdminAccess{Actor: "synthetic-admin", Permissions: []string{account.PermViewLogs}})
	r := httptest.NewRequestWithContext(ctx, "GET", "/api/logs/stream?backfill="+strconv.Itoa(f.count), nil)
	w := &f.writer
	w.header, w.status, w.writes, w.flushes, w.bytes = make(http.Header), 0, 0, 0, 0
	w.readDeadlines, w.writeDeadlines, w.cancel = 0, 0, cancel
	f.queries, f.closed = 0, 0
	f.server.adminLogsStream(w, r)
	if w.status != 200 || w.header.Get("Content-Type") != "text/event-stream; charset=utf-8" || w.readDeadlines != 1 || w.writeDeadlines != w.writes || f.closed != f.queries || f.server.logStreams.total != 0 {
		tb.Fatalf("日志流状态、期限、响应关闭或名额释放发生变化：status=%d writes=%d deadlines=%d/%d queries=%d closed=%d", w.status, w.writes, w.readDeadlines, w.writeDeadlines, f.queries, f.closed)
	}
	if w.failWrite == 0 && w.failFlush == 0 && (w.writes != 3 || w.flushes != 3 || f.queries != 2) {
		tb.Fatalf("日志流没有完整写出并刷新三个帧：写出=%d 刷新=%d 查询=%d", w.writes, w.flushes, f.queries)
	}
}

func TestLogFrameStreamCompatibility(t *testing.T) {
	for _, tc := range []struct{ count, width int }{{1, 0}, {1, 1}, {1, 4096}, {200, 64}, {1000, 4096}} {
		t.Run(fmt.Sprintf("%dx%d", tc.count, tc.width), func(t *testing.T) { newLogFrameFixture(t, tc.count, tc.width).run(t) })
	}
	for i, message := range []string{"中文日志🙂", "引号\"、反斜线\\与换行\r\n\t<&>\u2028", strings.Repeat("界", 1400)} {
		t.Run(fmt.Sprintf("特殊正文=%d", i), func(t *testing.T) { newLogFrameMessageFixture(t, 2, message).run(t) })
	}
}

func TestLogFrameStreamWriteFailures(t *testing.T) {
	for _, flush := range []bool{false, true} {
		for stage := 1; stage <= 3; stage++ {
			t.Run(fmt.Sprintf("flush=%t/stage=%d", flush, stage), func(t *testing.T) {
				f := newLogFrameFixture(t, 1, 64)
				if flush {
					f.writer.failFlush = stage
				} else {
					f.writer.failWrite = stage
				}
				f.run(t)
				wantFlushes := stage - 1
				if flush {
					wantFlushes = stage
				}
				if f.writer.writes != stage || f.writer.flushes != wantFlushes || f.queries != stage-1 {
					t.Fatal("失败后仍继续写出或查询", f.writer.writes, f.writer.flushes, f.queries)
				}
			})
		}
	}
}

func BenchmarkLogFrameStream(b *testing.B) {
	for _, tc := range []struct{ count, width int }{{1, 64}, {200, 64}, {200, 4096}, {1000, 4096}} {
		b.Run(fmt.Sprintf("%dx%d", tc.count, tc.width), func(b *testing.B) {
			f := newLogFrameFixture(b, tc.count, tc.width)
			f.run(b)
			f.writer.check = false
			b.ReportAllocs()
			b.SetBytes(int64(tc.count * tc.width))
			for b.Loop() {
				f.run(b)
			}
		})
	}
}

func TestLogStreamFrameBytes(t *testing.T) {
	for _, event := range []string{"lines", "pods", "gap", "error", "end"} {
		for _, id := range []string{"", "1700000000000000000"} {
			for _, size := range []int{0, 1, 2, 31, 32, 63, 64, 65, 127, 128, 4096, 4097} {
				content := bytes.Repeat([]byte("x"), size)
				want := ""
				if id != "" {
					want = "id: " + id + "\n"
				}
				want += "event: " + event + "\ndata: " + string(content) + "\n\n"
				for _, capacity := range []int{size, len(want) - 1, len(want), len(want) + 64} {
					storage := make([]byte, capacity)
					copy(storage, content)
					got := logStreamFrame(event, id, storage[:size])
					if string(got) != want {
						t.Fatalf("帧字节变化：event=%q id=%q size=%d capacity=%d", event, id, size, capacity)
					}
					if capacity >= len(want) && &got[0] != &storage[0] {
						t.Fatal("容量充足时没有复用独占缓冲")
					}
					if capacity < len(want) && !bytes.Equal(storage[:size], content) {
						t.Fatal("另建帧时修改了原正文")
					}
				}
			}
		}
	}
}

func TestLogStreamFrameBufferBudget(t *testing.T) {
	for _, size := range []int{32, 1 << 20} {
		for _, enough := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes=%d/reuse=%t", size, enough), func(t *testing.T) {
				content := bytes.Repeat([]byte("x"), size)
				capacity := size
				budget := float64(1)
				if enough {
					capacity += 128
					budget = 0
				}
				storage := make([]byte, capacity)
				var got []byte
				allocations := testing.AllocsPerRun(10, func() {
					copy(storage, content)
					got = logStreamFrame("lines", "1700000000000000000", storage[:size])
				})
				if len(got) <= size {
					t.Fatal("完整帧没有包含正文")
				}
				if allocations > budget {
					t.Fatalf("帧构造分配 %.0f 次，预算 %.0f 次", allocations, budget)
				}
			})
		}
	}
}
