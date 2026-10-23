package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cliInlineOutputWriter struct {
	bytes.Buffer
	calls int
	limit int
	cause error
}

func (w *cliInlineOutputWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.limit >= 0 {
		p = p[:min(len(p), w.limit)]
	}
	n, _ := w.Buffer.Write(p)
	return n, w.cause
}

func TestCLIInlineOutputCompatibility(t *testing.T) {
	large := strings.Repeat("x", 8<<20)
	for _, tc := range []struct {
		name   string
		kind   string
		data   string
		want   string
		status int
	}{
		{name: "空文本", kind: "text/plain"},
		{name: "空JSON", kind: "application/json"},
		{name: "文本", kind: "text/plain", data: "合成文本", want: "合成文本\n"},
		{name: "多个LF", kind: "text/plain", data: "synthetic\n\n", want: "synthetic\n"},
		{name: "全部LF", kind: "text/plain", data: "\n\n", want: "\n"},
		{name: "保留CR", kind: "text/plain", data: "synthetic\r\n\n", want: "synthetic\r\n"},
		{name: "保留空格", kind: "text/plain", data: "synthetic\n \n", want: "synthetic\n \n"},
		{name: "保留无效UTF8", kind: "text/plain", data: "\xff\xfe\n\n", want: "\xff\xfe\n"},
		{name: "大文本末尾补LF", kind: "text/plain", data: large, want: large + "\n"},
		{name: "JSON缩进", kind: "application/json", data: `{"items":[1,true]}`, want: "{\n  \"items\": [\n    1,\n    true\n  ]\n}\n"},
		{name: "JSON后缀", kind: "application/problem+json; charset=utf-8", data: "{\"ok\":true}\n\n", want: "{\n  \"ok\": true\n}\n"},
		{name: "JSON字符串", kind: "application/json", data: `"synthetic"`, want: "\"synthetic\"\n"},
		{name: "无效JSON", kind: "application/json", data: "invalid json\n\n", want: "invalid json\n"},
		{name: "纯空白JSON", kind: "application/json", data: " \n\n", want: " \n"},
		{name: "无效UTF8JSON", kind: "application/json", data: "\xff\n\n", want: "\xff\n"},
		{name: "文本状态错误", kind: "text/plain", data: "synthetic failure\n\n", want: "synthetic failure\n", status: http.StatusBadGateway},
		{name: "JSON状态错误", kind: "application/json", data: `{"ok":false}`, want: "{\n  \"ok\": false\n}\n", status: http.StatusBadGateway},
	} {
		for _, known := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/known=%v", tc.name, known), func(t *testing.T) {
				body := &cliResponseBufferBody{reader: bytes.NewReader([]byte(tc.data))}
				writer := &cliInlineOutputWriter{limit: -1}
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				length := int64(-1)
				if known {
					length = int64(len(tc.data))
				}
				err := (cli{stdout: writer}).print(&http.Response{StatusCode: status, Status: fmt.Sprint(status), ContentLength: length, Body: body, Header: http.Header{"Content-Type": {tc.kind}}}, "")
				if tc.status == 0 {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var statusErr statusError
					if !errors.As(err, &statusErr) || statusErr.status != fmt.Sprint(status) {
						t.Fatalf("状态错误变化：%v", err)
					}
				}
				wantCalls := 0
				if tc.data != "" {
					wantCalls = 1
				}
				if writer.String() != tc.want || writer.calls != wantCalls || !body.closed || body.read != len(tc.data) {
					t.Fatalf("输出语义变化：输出=%q，写入次数=%d，关闭=%v，读取=%d", writer.String(), writer.calls, body.closed, body.read)
				}
			})
		}
	}
}

func TestCLIInlineOutputWriteErrors(t *testing.T) {
	writeError := errors.New("synthetic output failure")
	for _, kind := range []string{"text/plain", "application/json"} {
		for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
			for _, tc := range []struct {
				name  string
				limit int
				cause error
			}{
				{"完整写入后错误", -1, writeError},
				{"部分写入错误", 3, writeError},
				{"未写入错误", 0, io.ErrClosedPipe},
				{"保留无错误短写行为", 3, nil},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", kind, status, tc.name), func(t *testing.T) {
					data := "synthetic"
					if kind == "application/json" {
						data = `"synthetic"`
					}
					body := &cliResponseBufferBody{reader: bytes.NewReader([]byte(data))}
					writer := &cliInlineOutputWriter{limit: tc.limit, cause: tc.cause}
					err := (cli{stdout: writer}).print(&http.Response{StatusCode: status, Status: fmt.Sprint(status), ContentLength: int64(len(data)), Body: body, Header: http.Header{"Content-Type": {kind}}}, "")
					if tc.cause != nil {
						if !errors.Is(err, tc.cause) {
							t.Fatalf("写入错误优先级或错误链变化：%v", err)
						}
					} else if status == http.StatusOK {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						var statusErr statusError
						if !errors.As(err, &statusErr) || statusErr.status != fmt.Sprint(status) {
							t.Fatalf("短写后的状态错误变化：%v", err)
						}
					}
					want := data + "\n"
					if tc.limit >= 0 {
						want = want[:tc.limit]
					}
					if writer.String() != want || writer.calls != 1 || !body.closed {
						t.Fatalf("错误输出或关闭行为变化：输出=%q，写入次数=%d，关闭=%v", writer.String(), writer.calls, body.closed)
					}
				})
			}
		}
	}
}

func benchmarkCLIInlineJSON(b *testing.B, size int, known, valid bool) {
	data := bytes.Repeat([]byte("x"), size)
	if valid {
		data[0], data[len(data)-1] = '"', '"'
	}
	length := int64(-1)
	if known {
		length = int64(size)
	}
	c := cli{stdout: io.Discard}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		response := &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Type": {"application/json"}}}
		if err := c.print(response, ""); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCLIInlineJSONAllocationBudget(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			const size = 8 << 20
			result := testing.Benchmark(func(b *testing.B) { benchmarkCLIInlineJSON(b, size, true, valid) })
			budget := int64(size)*7/2 + 128<<10
			if got := result.AllocedBytesPerOp(); got > budget {
				t.Fatalf("JSON 输出分配 %d B，预算 %d B", got, budget)
			}
		})
	}
}

func TestCLIInlineUnknownLengthAllocationBudget(t *testing.T) {
	const size = 8 << 20
	result := testing.Benchmark(func(b *testing.B) { benchmarkCLIResponseBuffer(b, size, false, false) })
	// 计入未知长度读取的分块分配，输出缓冲只需再分配最终长度。
	budget := int64(size)*21/4 + 128<<10
	if got := result.AllocedBytesPerOp(); got > budget {
		t.Fatalf("未知长度文本输出分配 %d B，预算 %d B", got, budget)
	}
}

func BenchmarkCLIInlineJSON(b *testing.B) {
	for _, valid := range []bool{false, true} {
		b.Run(fmt.Sprintf("valid=%v", valid), func(b *testing.B) {
			for _, tc := range []struct {
				name  string
				size  int
				known bool
			}{
				{"32B", 32, true}, {"512B", 512, true}, {"8MiB", 8 << 20, true}, {"16MiB", 16 << 20, true}, {"未知长度", 8 << 20, false},
			} {
				b.Run(tc.name, func(b *testing.B) { benchmarkCLIInlineJSON(b, tc.size, tc.known, valid) })
			}
		})
	}
}
