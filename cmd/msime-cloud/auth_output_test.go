package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func callCLIAuthOutput(c cli, postJSON bool) error {
	if postJSON {
		var result any
		return c.postJSON("https://example.test", "/v1/auth/challenges", nil, &result)
	}
	return c.signIn("https://example.test", "synthetic-challenge", "synthetic-credential")
}

func cliAuthOutputClient(body *cliResponseBufferBody, length int64, status int) *http.Client {
	return &http.Client{Transport: cliResponseBufferTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			r.Body.Close()
		}
		return &http.Response{StatusCode: status, Status: fmt.Sprint(status), ContentLength: length, Body: body, Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}
}

type cliAuthOutputWriter struct {
	cliInlineOutputWriter
	body          *cliResponseBufferBody
	closedAtWrite bool
}

func (w *cliAuthOutputWriter) Write(p []byte) (int, error) {
	w.closedAtWrite = w.body.closed
	return w.cliInlineOutputWriter.Write(p)
}

func TestCLIAuthOutputFailureCompatibility(t *testing.T) {
	readError, writeError := errors.New("synthetic read failure"), errors.New("synthetic write failure")
	for _, postJSON := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			data       string
			want       string
			readError  error
			writeError error
			limit      int
		}{
			{name: "空正文", want: "\n", limit: -1},
			{name: "纯空白", data: " \t\r\n", want: "\n", limit: -1},
			{name: "Unicode空白", data: "\u00a0\u2003合成错误\u3000\n", want: "合成错误\n", limit: -1},
			{name: "保持JSON原始格式", data: " \n{\"error\":true}\r\n", want: "{\"error\":true}\n", limit: -1},
			{name: "无效JSON", data: " invalid json \n", want: "invalid json\n", limit: -1},
			{name: "无效UTF8", data: " \xff\xfe\n", want: "\xff\xfe\n", limit: -1},
			{name: "保留中间空白", data: "\nsynthetic\n \nerror\n", want: "synthetic\n \nerror\n", limit: -1},
			{name: "保留零宽空格", data: "\u200bsynthetic\u200b", want: "\u200bsynthetic\u200b\n", limit: -1},
			{name: "保留NUL", data: "\x00synthetic\x00", want: "\x00synthetic\x00\n", limit: -1},
			{name: "读取错误优先", data: "synthetic", readError: readError, limit: -1},
			{name: "完整写入后错误", data: "synthetic", want: "synthetic\n", writeError: writeError, limit: -1},
			{name: "部分写入错误", data: "synthetic", want: "syn", writeError: writeError, limit: 3},
			{name: "未写入错误", data: "synthetic", writeError: io.ErrClosedPipe},
			{name: "保留无错误短写行为", data: "synthetic", want: "syn", limit: 3},
		} {
			t.Run(fmt.Sprintf("postJSON=%v/%s", postJSON, tc.name), func(t *testing.T) {
				body := &cliResponseBufferBody{reader: bytes.NewReader([]byte(tc.data)), cause: tc.readError}
				writer := &cliAuthOutputWriter{cliInlineOutputWriter: cliInlineOutputWriter{limit: tc.limit, cause: tc.writeError}, body: body}
				c := cli{stdout: writer, client: cliAuthOutputClient(body, 4096, http.StatusServiceUnavailable)}
				err := callCLIAuthOutput(c, postJSON)
				switch {
				case tc.readError != nil:
					if !errors.Is(err, tc.readError) || writer.calls != 0 {
						t.Fatalf("读取错误处理变化：错误=%v，写入次数=%d", err, writer.calls)
					}
				case tc.writeError != nil:
					if !errors.Is(err, tc.writeError) {
						t.Fatalf("写入错误链或优先级变化：%v", err)
					}
				default:
					var statusErr statusError
					if !errors.As(err, &statusErr) || statusErr.status != "503" {
						t.Fatalf("状态错误变化：%v", err)
					}
				}
				if writer.String() != tc.want || !body.closed || body.read != len(tc.data) {
					t.Fatalf("输出或关闭行为变化：输出=%q，关闭=%v，读取=%d", writer.String(), body.closed, body.read)
				}
				if tc.readError == nil && (writer.calls != 1 || writer.closedAtWrite != postJSON) {
					t.Fatalf("写入次数或关闭时序变化：写入次数=%d，写入前关闭=%v", writer.calls, writer.closedAtWrite)
				}
			})
		}
	}
}

func TestCLIAuthOutputSuccessCompatibility(t *testing.T) {
	for _, user := range []string{`{"id":"synthetic-user","display_name":"合成用户"}`, `null`} {
		for _, failWrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("user=%s/failWrite=%v", user, failWrite), func(t *testing.T) {
				data := []byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":900,"user":` + user + `}`)
				body := &cliResponseBufferBody{reader: bytes.NewReader(data)}
				writer := &cliAuthOutputWriter{cliInlineOutputWriter: cliInlineOutputWriter{limit: -1}, body: body}
				writeError := errors.New("synthetic output failure")
				if failWrite {
					writer.cause = writeError
				}
				dir := t.TempDir()
				c := cli{stdout: writer, client: cliAuthOutputClient(body, int64(len(data)), http.StatusOK), env: func(string) string { return dir }, now: func() time.Time { return time.Unix(1700000000, 0) }}
				err := callCLIAuthOutput(c, false)
				if failWrite && !errors.Is(err, writeError) || !failWrite && err != nil {
					t.Fatalf("用户输出错误变化：%v", err)
				}
				want := "null\n"
				if user != "null" {
					want = "{\n  \"id\": \"synthetic-user\",\n  \"display_name\": \"合成用户\"\n}\n"
				}
				saved, ok, err := (store{dir: dir}).get("https://example.test")
				if err != nil || !ok || saved.AccessToken != "synthetic-access" || saved.RefreshToken != "synthetic-refresh" {
					t.Fatalf("输出前保存会话的行为变化：存在=%v，错误=%v", ok, err)
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, saved.User); err != nil || compact.String() != user {
					t.Fatalf("保存的用户信息变化：错误=%v", err)
				}
				if writer.String() != want || writer.calls != 1 || writer.closedAtWrite || !body.closed {
					t.Fatalf("用户输出或关闭行为变化：输出=%q，写入次数=%d，写入前关闭=%v，关闭=%v", writer.String(), writer.calls, writer.closedAtWrite, body.closed)
				}
			})
		}
	}
}

func benchmarkCLIAuthOutput(b *testing.B, size int, known, postJSON, success bool) {
	data := bytes.Repeat([]byte("x"), size)
	status := http.StatusServiceUnavailable
	if success {
		const prefix = `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":900,"user":{"display_name":"`
		const suffix = `"}}`
		copy(data, prefix)
		copy(data[len(data)-len(suffix):], suffix)
		status = http.StatusOK
	}
	length := int64(-1)
	if known {
		length = int64(size)
	}
	dir := b.TempDir()
	c := cli{stdout: io.Discard, env: func(string) string { return dir }, now: func() time.Time { return time.Unix(1700000000, 0) }, client: &http.Client{Transport: cliResponseBufferTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			r.Body.Close()
		}
		return &http.Response{StatusCode: status, Status: fmt.Sprint(status), ContentLength: length, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		if success {
			// 每次测量首次登录；清理上一轮凭据不计入时间和分配。
			b.StopTimer()
			if err := os.Remove(filepath.Join(dir, "credentials.json")); err != nil && !os.IsNotExist(err) {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		err := callCLIAuthOutput(c, postJSON)
		if success {
			if err != nil {
				b.Fatal(err)
			}
		} else {
			var statusErr statusError
			if !errors.As(err, &statusErr) || statusErr.status != "503" {
				b.Fatalf("错误响应处理变化：%v", err)
			}
		}
	}
}

func TestCLIAuthOutputAllocationBudget(t *testing.T) {
	for _, postJSON := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			t.Run(fmt.Sprintf("postJSON=%v/known=%v", postJSON, known), func(t *testing.T) {
				const size = 8 << 20
				result := testing.Benchmark(func(b *testing.B) { benchmarkCLIAuthOutput(b, size, known, postJSON, false) })
				budget := int64(size)*3/2 + 256<<10
				if !known {
					budget = int64(size)*11/2 + 256<<10
				}
				if got := result.AllocedBytesPerOp(); got > budget {
					t.Fatalf("鉴权错误输出分配 %d B，预算 %d B", got, budget)
				}
			})
		}
	}
}

func TestCLIAuthOutputSuccessAllocationBudget(t *testing.T) {
	const size = 8 << 20
	result := testing.Benchmark(func(b *testing.B) { benchmarkCLIAuthOutput(b, size, true, false, true) })
	budget := int64(size)*13 + 1<<20
	if got := result.AllocedBytesPerOp(); got > budget {
		t.Fatalf("成功登录输出分配 %d B，预算 %d B", got, budget)
	}
}

func BenchmarkCLIAuthOutput(b *testing.B) {
	for _, mode := range []struct {
		name     string
		postJSON bool
		success  bool
	}{
		{"登录失败", false, false}, {"JSON请求失败", true, false}, {"登录成功", false, true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			for _, tc := range []struct {
				name  string
				size  int
				known bool
			}{
				{"32B", 32, true}, {"512B", 512, true}, {"8MiB", 8 << 20, true}, {"16MiB", 16 << 20, true}, {"未知长度", 8 << 20, false},
			} {
				if mode.success && (tc.size < 512 || tc.size == 16<<20) {
					continue
				}
				b.Run(tc.name, func(b *testing.B) { benchmarkCLIAuthOutput(b, tc.size, tc.known, mode.postJSON, mode.success) })
			}
		})
	}
}
