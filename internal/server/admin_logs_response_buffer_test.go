package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestLokiResponseBufferCompatibility(t *testing.T) {
	privateFailure := errors.New("synthetic-private-read-details")
	valid := []byte(`{"status":"success"}`)
	for _, tc := range []struct {
		name    string
		data    []byte
		length  int64
		status  int
		cause   error
		wantErr string
	}{
		{name: "小响应", data: valid, length: int64(len(valid))},
		{name: "准确长度", data: append(bytes.Clone(valid), bytes.Repeat([]byte(" "), 4096-len(valid))...), length: 4096},
		{name: "未知长度", data: valid, length: -1},
		{name: "零长度", data: valid},
		{name: "少报长度", data: append(bytes.Clone(valid), bytes.Repeat([]byte(" "), 4096)...), length: 512},
		{name: "多报长度", data: valid, length: 4096},
		{name: "声明超过池上限", data: valid, length: maxLokiPooledResponseBytes + 1},
		{name: "声明超限但正文合法", data: valid, length: maxLokiResponseBytes + 1},
		{name: "极大声明", data: valid, length: 1<<63 - 1},
		{name: "恰好上限", data: append(bytes.Clone(valid), bytes.Repeat([]byte(" "), maxLokiResponseBytes-len(valid))...), length: maxLokiResponseBytes},
		{name: "实际超限", data: bytes.Repeat([]byte(" "), maxLokiResponseBytes+32), length: 512, wantErr: "response too large"},
		{name: "空正文", length: 512, wantErr: "invalid response"},
		{name: "无效JSON", data: []byte("synthetic-private-invalid-response"), length: 512, wantErr: "invalid response"},
		{name: "尾随JSON", data: []byte(`{} {}`), length: 512, wantErr: "invalid response"},
		{name: "失败状态不读取正文", data: []byte("synthetic-private-status-response"), length: 4096, status: http.StatusBadGateway, wantErr: "status 502"},
		{name: "读取错误", data: valid, length: 512, cause: privateFailure, wantErr: "read failed"},
		{name: "数据与EOF", data: valid, length: 512, cause: io.EOF},
		{name: "截断", data: valid, length: 4096, cause: io.ErrUnexpectedEOF, wantErr: "read failed"},
		{name: "超时", data: valid, length: 512, cause: context.DeadlineExceeded, wantErr: "read failed"},
		{name: "取消", length: 512, cause: context.Canceled, wantErr: "read failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &upstreamBufferBody{reader: bytes.NewReader(tc.data), cause: tc.cause}
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			s := &Server{loki: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, ContentLength: tc.length, Body: body}, nil
			})}}
			s.config.Admin.Logs.LokiURL = "https://loki.example.test"
			var got struct {
				Status string `json:"status"`
			}
			err := s.lokiGet(t.Context(), "/loki/api/v1/query_range", nil, &got)
			if tc.wantErr == "" {
				if err != nil || got.Status != "success" {
					t.Fatalf("正文解析变化：状态=%q，错误=%v", got.Status, err)
				}
			} else if err != (errLoki{tc.wantErr}) || errors.Is(err, privateFailure) || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatalf("错误分类或脱敏变化：%v", err)
			}
			wantRead := min(len(tc.data), maxLokiResponseBytes+1)
			if tc.status != 0 {
				wantRead = 0
			}
			if body.read != wantRead || !body.closed {
				t.Fatalf("读取上限或关闭变化：读取=%d，期望=%d，关闭=%v", body.read, wantRead, body.closed)
			}
		})
	}
}

func lokiResponseBufferPayload(count, width int) ([]byte, string) {
	line := strings.Repeat("x", width)
	values := make([][2]string, count)
	for i := range values {
		values[i] = [2]string{strconv.FormatInt(1700000000000000000+int64(i), 10), line}
	}
	data, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": []any{map[string]any{"stream": map[string]string{"pod": "synthetic-pod"}, "values": values}}}})
	if err != nil {
		panic(err)
	}
	return data, line
}

func benchmarkLokiResponseBuffer(b *testing.B, count, width int, known bool) {
	data, line := lokiResponseBufferPayload(count, width)
	length := int64(-1)
	if known {
		length = int64(len(data))
	}
	s := &Server{loki: &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}}
	s.config.Admin.Logs.LokiURL = "https://loki.example.test"
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		got, err := s.lokiQueryRange(context.Background(), "{}", 1, 1800000000000000000, maxLogLimit, "forward")
		if err != nil || len(got) != count || got[0].line != line || got[count-1].line != line || got[0].pod != "synthetic-pod" {
			b.Fatalf("日志查询输出变化：数量=%d，错误=%v", len(got), err)
		}
	}
}

func TestLokiResponseBufferAllocationBudget(t *testing.T) {
	for _, width := range []int{4096, 8192} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			data, _ := lokiResponseBufferPayload(maxLogLimit, width)
			// 预算包含正文、两千条日志的 JSON 解码及拍平结果，不只测读取器。
			budget := int64(len(data))*5/2 + 512<<10
			result := testing.Benchmark(func(b *testing.B) { benchmarkLokiResponseBuffer(b, maxLogLimit, width, true) })
			if got := result.AllocedBytesPerOp(); got > budget {
				t.Fatalf("日志查询分配 %d B，预算 %d B", got, budget)
			}
		})
	}
}

func BenchmarkLokiResponseBuffer(b *testing.B) {
	for _, tc := range []struct {
		name  string
		count int
		width int
		known bool
	}{
		{"小响应", 1, 32, true},
		{"常见池内响应", defaultLogLimit, 128, true},
		{"两千条4KiB日志", maxLogLimit, 4096, true},
		{"两千条8KiB日志", maxLogLimit, 8192, true},
		{"未知长度", maxLogLimit, 4096, false},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkLokiResponseBuffer(b, tc.count, tc.width, tc.known) })
	}
}
