package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func diagnosticRecordRequest() *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name: "read_crash_logs", Arguments: json.RawMessage(`{"limit":1}`),
	}}
}

func diagnosticRecordCrashFixture() CrashLogsOutput {
	return CrashLogsOutput{CrashLogs: []account.DiagnosticCrash{{
		At: "2026-10-01T00:00:00Z", Message: "合成崩溃", Stack: strings.Repeat("synthetic_frame ", 1024),
	}}}
}

func benchmarkDiagnosticRecord(b *testing.B, out any, count int) {
	store := &memorySnapshots{}
	req := diagnosticRecordRequest()
	want, err := json.Marshal(out)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		// 复用访问记录槽位，避免把测试存储的切片增长算作被测路径开销。
		store.accesses = store.accesses[:0]
		if err := record(ctx, store, snapshotID, req, count, out); err != nil {
			b.Fatal(err)
		}
		if len(store.accesses) != 1 || store.accesses[0].count != count || store.accesses[0].bytes != len(want) {
			b.Fatal("访问记录的条数或 JSON 字节数不正确")
		}
	}
}

func BenchmarkDiagnosticRecord(b *testing.B) {
	for _, tc := range []struct {
		name  string
		out   any
		count int
	}{
		{"8192条事件", EventsOutput{Events: events(diagnosticEventFixture(), EventInput{})}, 8192},
		{"长调用栈", diagnosticRecordCrashFixture(), 1},
		{"设置快照", ConfigOutput{Config: map[string]any{"theme": "system", "api_token": account.DiagnosticRedacted}}, 1},
		{"空事件", EventsOutput{Events: []Event{}}, 0},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkDiagnosticRecord(b, tc.out, tc.count)
		})
	}
}

func TestDiagnosticRecordAllocationBudget(t *testing.T) {
	body, err := json.Marshal(diagnosticRecordCrashFixture())
	if err != nil {
		t.Fatal(err)
	}
	// 预编码合成结果，单独约束计数路径的整份复制，避开对象遍历时的缓冲增长。
	result := testing.Benchmark(func(b *testing.B) {
		benchmarkDiagnosticRecord(b, json.RawMessage(body), 1)
	})
	t.Logf("诊断访问记录分配 %d B", result.AllocedBytesPerOp())
	// 为竞态构建中编码池的随机丢弃留余量，仍应避免为计数复制整份调用栈 JSON。
	if allocated := result.AllocedBytesPerOp(); allocated > 16<<10 {
		t.Fatalf("诊断访问记录分配 %d B，超过 16 KiB 预算", allocated)
	}
}

func TestDiagnosticRecordJSONBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  any
	}{
		{"空值", nil},
		{"空事件", EventsOutput{Events: []Event{}}},
		{"未初始化事件", EventsOutput{}},
		{"事件数字与可选耗时", EventsOutput{Events: []Event{{TMS: 0, Kind: "key_down"}, {TMS: math.MaxInt64, Kind: "commit", DurationMS: int64Pointer(1)}}}},
		{"长调用栈", diagnosticRecordCrashFixture()},
		{"崩溃转义", CrashLogsOutput{CrashLogs: []account.DiagnosticCrash{{At: "2026-10-01T00:00:00Z", Message: "合成<&>\"\\\n\t\x00\u2028\u2029", Stack: "frame\xff"}}}},
		{"设置嵌套", ConfigOutput{Config: map[string]any{"合成<&>": []any{nil, true, 1.25, map[string]any{"api_token": account.DiagnosticRedacted, "line": "\n"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := json.Marshal(tc.out)
			if err != nil {
				t.Fatal(err)
			}
			store := &memorySnapshots{}
			req := diagnosticRecordRequest()
			if err := record(t.Context(), store, snapshotID, req, 3, tc.out); err != nil {
				t.Fatal(err)
			}
			if len(store.accesses) != 1 {
				t.Fatalf("访问记录条数=%d，期望=1", len(store.accesses))
			}
			if got := store.accesses[0]; !reflect.DeepEqual(got, access{req.Params.Name, string(req.Params.Arguments), 3, len(want)}) {
				t.Fatalf("访问记录字段不正确：实际=%+v，期望字节数=%d", got, len(want))
			}
		})
	}
}

func TestDiagnosticRecordEncodingFailure(t *testing.T) {
	for _, out := range []any{
		make(chan int), ConfigOutput{Config: map[string]any{"number": math.NaN()}},
	} {
		store := &memorySnapshots{}
		if err := record(t.Context(), store, snapshotID, diagnosticRecordRequest(), 1, out); err == nil {
			t.Fatal("JSON 编码失败时仍返回成功")
		}
		if len(store.accesses) != 0 {
			t.Fatal("JSON 编码失败时不应写入成功访问记录")
		}
	}
}

func TestDiagnosticRecordStoreFailure(t *testing.T) {
	store := &memorySnapshots{recordErr: errors.New("synthetic storage failure")}
	err := record(t.Context(), store, snapshotID, diagnosticRecordRequest(), 1, diagnosticRecordCrashFixture())
	if err == nil || err.Error() != "access log unavailable" {
		t.Fatalf("访问记录写入失败时未返回固定错误：%v", err)
	}
}
