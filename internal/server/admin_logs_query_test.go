package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type lokiQueryResponseTransport string

func (body lokiQueryResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

type lokiReadErrorBody struct{}

func (lokiReadErrorBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (lokiReadErrorBody) Close() error             { return nil }

type lokiReadErrorTransport struct{}

func (lokiReadErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: lokiReadErrorBody{}}, nil
}

func fixedLokiQueryServer(body string) *Server {
	s := &Server{loki: &http.Client{Transport: lokiQueryResponseTransport(body)}}
	s.config.Admin.Logs.LokiURL = "http://loki.example.test"
	return s
}

func lokiQueryPayload(sizes ...int) (string, []lokiEntry) {
	type stream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	streams := make([]stream, len(sizes))
	var entries []lokiEntry
	for i, size := range sizes {
		pod := fmt.Sprintf("synthetic-pod-%d", i)
		streams[i] = stream{Stream: map[string]string{"pod": pod}, Values: make([][2]string, size)}
		for j := range streams[i].Values {
			ns := int64(1700000000000000000) + int64(j)
			line := "合成日志"
			streams[i].Values[j] = [2]string{strconv.FormatInt(ns, 10), line}
			entries = append(entries, lokiEntry{ns: ns, pod: pod, line: line})
		}
	}
	data, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": streams}})
	if err != nil {
		panic(err)
	}
	return string(data), entries
}

func TestLokiQueryRangeFlattening(t *testing.T) {
	for _, sizes := range [][]int{nil, {0}, {1}, {2, 0, 3}, {maxLogLimit}, {1, 1, 1}} {
		t.Run(fmt.Sprint(sizes), func(t *testing.T) {
			payload, want := lokiQueryPayload(sizes...)
			got, err := fixedLokiQueryServer(payload).lokiQueryRange(context.Background(), "{}", 1, math.MaxInt64, maxLogLimit, "forward")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("日志流展开结果变化：got=%+v want=%+v err=%v", got, want, err)
			}
		})
	}
}

func TestLokiQueryRangeInvalidTimestamp(t *testing.T) {
	for _, timestamp := range []string{"invalid", "9223372036854775808"} {
		payload := `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"pod":"synthetic"},"values":[["1","有效行"],["` + timestamp + `","无效行"]]}]}}`
		got, err := fixedLokiQueryServer(payload).lokiQueryRange(context.Background(), "{}", 0, 100, 10, "forward")
		if got != nil || err != (errLoki{"invalid timestamp"}) {
			t.Fatalf("无效时间戳返回了部分数据或错误变化：got=%+v err=%v", got, err)
		}
	}
}

func TestLokiQueryRangeAllocationBudget(t *testing.T) {
	for _, sizes := range [][]int{{maxLogLimit}, {900, 0, 1100}} {
		t.Run(fmt.Sprint(sizes), func(t *testing.T) {
			payload, want := lokiQueryPayload(sizes...)
			s := fixedLokiQueryServer(payload)
			var got []lokiEntry
			var err error
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, err = s.lokiQueryRange(context.Background(), "{}", 1, math.MaxInt64, maxLogLimit, "forward")
				}
			})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("日志查询结果变化：len=%d err=%v", len(got), err)
			}
			// race 模式会增加查询与解码分配；预算仍低于旧实现在该模式下约 912 KB 的开销。
			if allocated := result.AllocedBytesPerOp(); allocated > 800<<10 {
				t.Fatalf("两千行日志查询分配 %d 字节，超过 800 KiB 预算", allocated)
			}
		})
	}
}

func TestLokiQueryRangePooledResponseBudget(t *testing.T) {
	payload, want := lokiQueryPayload(maxLogLimit)
	s := fixedLokiQueryServer(payload)
	const queriesPerSample = 100
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			for range queriesPerSample {
				got, err := s.lokiQueryRange(context.Background(), "{}", 1, math.MaxInt64, maxLogLimit, "forward")
				if err != nil || len(got) != len(want) {
					b.Fatalf("日志查询结果变化：len=%d err=%v", len(got), err)
				}
			}
		}
	})
	// race 模式的 sync.Pool 会随机丢弃对象；每个样本至少测量一百次查询，保留全部冷分配并减少短采样波动。
	// 每次查询的预算保持 700 KiB，仍低于旧实现的约 912 KiB。
	if allocated := result.AllocedBytesPerOp() / queriesPerSample; allocated > 700<<10 {
		t.Fatalf("日志查询缓冲复用分配 %d 字节，超过 700 KiB 预算", allocated)
	}
}

func TestLokiGetReadError(t *testing.T) {
	s := fixedLokiQueryServer(`{}`)
	s.loki.Transport = lokiReadErrorTransport{}
	var result map[string]any
	if err := s.lokiGet(context.Background(), "/loki/api/v1/query", nil, &result); err != (errLoki{"read failed"}) {
		t.Fatalf("读取错误变化：%v", err)
	}
}

func TestLokiGetRejectsTrailingJSON(t *testing.T) {
	for _, body := range []string{"{} {}", "{}x", "{}\n[]"} {
		t.Run(body, func(t *testing.T) {
			s := fixedLokiQueryServer(body)
			var result map[string]any
			if err := s.lokiGet(context.Background(), "/loki/api/v1/query", nil, &result); err != (errLoki{"invalid response"}) {
				t.Fatalf("尾随 JSON 未被拒绝：%v", err)
			}
		})
	}
	// 与 json.Unmarshal 一样，合法空白可以出现在 JSON 后面。
	s := fixedLokiQueryServer("{} \n\t")
	var result map[string]any
	if err := s.lokiGet(context.Background(), "/loki/api/v1/query", nil, &result); err != nil {
		t.Fatalf("尾随空白被拒绝：%v", err)
	}
}

func BenchmarkLokiQueryRange(b *testing.B) {
	for _, size := range []int{0, defaultLogLimit, maxLogLimit} {
		payload, _ := lokiQueryPayload(size)
		s := fixedLokiQueryServer(payload)
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.lokiQueryRange(context.Background(), "{}", 1, math.MaxInt64, maxLogLimit, "forward"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
