package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer 让测试安全地收集并发写入的日志。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// requestLogLines 是收集到的请求日志，每行解析为 JSON 字段。
func (b *lockedBuffer) requestLogLines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(line, err)
		}
		if v["msg"] == "http request" {
			out = append(out, v)
		}
	}
	return out
}

func (b *lockedBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs 把默认 slog 换成写入缓冲区的 JSON 处理器，测试结束时还原。
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestRequestLogFields(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[ "SUCCESS", [ [ "secretpinyin", [ "你好" ] ] ] ]`))
	})
	logs := captureLogs(t)

	// 查询串和请求正文都不进日志，记录的是路由模式。
	w := call(s, "GET", "/v1/cloud/candidates?text=secretpinyin&scheme=pinyin", "")
	w2 := call(s, "POST", "/v1/translate", `{"text":"secret body text","source_lang":"zh","target_lang":"en"}`)
	lines := logs.requestLogLines(t)
	if len(lines) != 2 {
		t.Fatal(logs.String())
	}
	got := lines[0]
	if got["level"] != "INFO" || got["method"] != "GET" || got["route"] != "GET /v1/cloud/candidates" || got["status"] != float64(w.Code) || got["bytes"] != float64(w.Body.Len()) {
		t.Fatalf("%v", got)
	}
	if _, ok := got["duration_ms"].(float64); !ok {
		t.Fatalf("%v", got)
	}
	if lines[1]["route"] != "POST /v1/translate" || lines[1]["status"] != float64(w2.Code) {
		t.Fatalf("%v", lines[1])
	}
	for _, secret := range []string{"secretpinyin", "text=", "secret body", testToken, "Bearer", "127.0.0.1", "192.0.2"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("%q leaked into the request log: %s", secret, logs.String())
		}
	}
	// 每行只有这几个字段，没有 IP、用户 ID 或令牌。
	for key := range got {
		switch key {
		case "time", "level", "msg", "method", "route", "status", "duration_ms", "bytes":
		default:
			t.Fatalf("unexpected field %s", key)
		}
	}
}

func TestRequestLogRoutesAndLevels(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app", token: strings.Repeat("a", 40), tokenHash: sha256.Sum256([]byte(strings.Repeat("a", 40)))}
	s.config.Admin.Logs = AdminLogsConfig{LokiURL: "http://loki.example.test", Selector: defaultLogSelector}
	logs := captureLogs(t)
	serve := func(r *http.Request) {
		s.ServeHTTP(httptest.NewRecorder(), r)
	}
	// 健康检查不记。
	serve(httptest.NewRequest("GET", "/healthz", nil))
	if lines := logs.requestLogLines(t); len(lines) != 0 {
		t.Fatal(lines)
	}
	// 中间件里被拒绝的请求（缺令牌）没有到达多路复用器，也能查到路由模式。
	serve(httptest.NewRequest("GET", "/v1/cloud/candidates?text=hidden", nil))
	// 上游失败的 5xx 记为 WARN。
	call(s, "GET", "/v1/cloud/candidates?text=hidden", "")
	serve(httptest.NewRequest("GET", "/no/such/path", nil))
	serve(httptest.NewRequest("BREW", "/v1/capabilities", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/api/users/u-1234567", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/api/Bad%20Seg/x", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/api/system", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/api/auth/session", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/api/site-settings", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/assets/app.js", nil))
	serve(httptest.NewRequest("GET", "https://admin.msime.app/users", nil))
	serve(httptest.NewRequest("GET", "/swagger", nil))
	// 日志流本身不记。
	serve(httptest.NewRequest("GET", "https://admin.msime.app/api/logs/stream", nil))
	want := []struct {
		level, method, route string
		status               float64
	}{
		{"INFO", "GET", "GET /v1/cloud/candidates", 401},
		{"WARN", "GET", "GET /v1/cloud/candidates", 502},
		{"INFO", "GET", "unmatched", 401},
		{"INFO", "OTHER", "unmatched", 401},
		{"INFO", "GET", "admin:/api/users/*", 401},
		{"INFO", "GET", "admin:/api/?", 401},
		{"INFO", "GET", "admin:/api/system", 401},
		{"INFO", "GET", "admin:/api/auth/*", 200},
		{"INFO", "GET", "admin:/api/site-settings", 401},
		{"INFO", "GET", "admin:asset", 404},
		{"INFO", "GET", "admin:page", 200},
		{"INFO", "GET", "docs", 404},
	}
	lines := logs.requestLogLines(t)
	if len(lines) != len(want) {
		t.Fatal(logs.String())
	}
	for i, w := range want {
		got := lines[i]
		if got["level"] != w.level || got["method"] != w.method || got["route"] != w.route || got["status"] != w.status {
			t.Fatalf("%d: %v, want %+v", i, got, w)
		}
	}
	if strings.Contains(logs.String(), "u-1234567") || strings.Contains(logs.String(), "hidden") {
		t.Fatal("path parameters or query leaked:", logs.String())
	}

	// 配置关闭后不再写请求日志。
	off, on := false, true
	for _, tc := range []struct {
		value *bool
		want  bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		c := Config{Clients: []Client{{ID: "test", TokenEnv: "TEST_CLIENT_TOKEN", RequestsPerMinute: 100}}, RequestLog: tc.value}
		if err := c.Validate(); err != nil || c.logRequests != tc.want {
			t.Fatal(tc.value, c.logRequests, err)
		}
	}
	s.config.logRequests = false
	logs.reset()
	call(s, "GET", "/v1/capabilities", "")
	if lines := logs.requestLogLines(t); len(lines) != 0 {
		t.Fatal(lines)
	}
}

func TestRequestLogUnknownAdminSectionPrivacy(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	s.config.Admin = AdminConfig{Enabled: true, Host: "admin.msime.app"}
	logs := captureLogs(t)
	for _, path := range []string{
		"/api/private-secret",
		"/api/private-secret/details",
		"/api/private_secret_2/details",
		"/api/%70rivate-secret/details",
	} {
		logs.reset()
		r := httptest.NewRequest(http.MethodGet, "https://admin.msime.app"+path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		lines := logs.requestLogLines(t)
		if w.Code != http.StatusUnauthorized || len(lines) != 1 {
			t.Fatalf("未知后台路径请求结果错误：status=%d，日志=%v", w.Code, lines)
		}
		if lines[0]["route"] != "admin:/api/?" {
			t.Errorf("未知路径首段进入路由标签：%v", lines[0]["route"])
		}
		if strings.Contains(logs.String(), "private") {
			t.Errorf("未经认证的未知路径泄露到日志：%s", logs.String())
		}
	}
}

func TestRequestRecorder(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &requestRecorder{ResponseWriter: inner}
	rec.WriteHeader(http.StatusAccepted)
	rec.WriteHeader(http.StatusTeapot)
	if rec.status != http.StatusAccepted {
		t.Fatal(rec.status)
	}
	if _, err := rec.Write([]byte("abc")); err != nil || rec.bytes != 3 {
		t.Fatal(rec.bytes, err)
	}
	// 包装后仍能经 ResponseController 找到底层的 Flush。
	if err := http.NewResponseController(rec).Flush(); err != nil || !inner.Flushed {
		t.Fatal(err)
	}
	implicit := &requestRecorder{ResponseWriter: httptest.NewRecorder()}
	_, _ = implicit.Write([]byte("x"))
	if implicit.status != http.StatusOK {
		t.Fatal(implicit.status)
	}
}

func TestRequestRecorderInformationalResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []int
		body    string
		status  int
	}{
		{"implicit after hints", []int{http.StatusEarlyHints}, "abc", http.StatusOK},
		{"explicit after hints", []int{http.StatusContinue, http.StatusEarlyHints, http.StatusAccepted}, "abc", http.StatusAccepted},
		{"switching protocols is final", []int{http.StatusSwitchingProtocols, http.StatusAccepted}, "", http.StatusSwitchingProtocols},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type recorded struct {
				status int
				bytes  int64
				err    error
			}
			done := make(chan recorded, 1)
			// 真实 HTTP 服务支持多次 1xx；httptest.ResponseRecorder 只保留首个状态。
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec := &requestRecorder{ResponseWriter: w}
				for _, status := range tc.headers {
					rec.WriteHeader(status)
				}
				var err error
				if tc.body != "" {
					_, err = rec.Write([]byte(tc.body))
				}
				done <- recorded{rec.status, rec.bytes, err}
			}))
			srv.Config.ErrorLog = log.New(io.Discard, "", 0)
			srv.Start()
			defer srv.Close()
			res, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			got := <-done
			if got.err != nil || got.status != tc.status || got.bytes != int64(len(tc.body)) || res.StatusCode != tc.status {
				t.Fatalf("记录=%+v，HTTP 状态=%d，期望=%d", got, res.StatusCode, tc.status)
			}
		})
	}
}

// 请求日志的开销：对比不记日志的 /healthz、记一行的 /v1/capabilities 和关闭请求日志后的 /v1/capabilities。
func BenchmarkRequestLog(b *testing.B) {
	b.Setenv("BENCH_TOKEN", testToken)
	s, err := New(Config{Clients: []Client{{ID: "bench", TokenEnv: "BENCH_TOKEN", RequestsPerMinute: 100000}}})
	if err != nil {
		b.Fatal(err)
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)
	for _, tc := range []struct {
		name, path string
		logged     bool
	}{{"healthz", "/healthz", true}, {"logged", "/v1/capabilities", true}, {"off", "/v1/capabilities", false}} {
		b.Run(tc.name, func(b *testing.B) {
			s.config.logRequests = tc.logged
			r := httptest.NewRequest("GET", tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+testToken)
			b.ReportAllocs()
			for b.Loop() {
				s.ServeHTTP(httptest.NewRecorder(), r)
			}
		})
	}
}
