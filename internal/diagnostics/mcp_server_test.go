package diagnostics

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var diagnosticServerBenchmarkSink *mcp.Server

func benchmarkDiagnosticServer(b *testing.B, store *memorySnapshots) {
	// 预热静态类型元数据，只测量后续请求的新 server 注册开销。
	_ = NewServer(store, store.snapshot)
	b.Cleanup(func() { diagnosticServerBenchmarkSink = nil })
	b.ReportAllocs()
	for b.Loop() {
		diagnosticServerBenchmarkSink = NewServer(store, store.snapshot)
	}
}

func diagnosticServerConfigFixture() *memorySnapshots {
	store := fixture(false)
	store.snapshot.Sections = account.DiagnosticSections{ConfigSnapshot: map[string]any{"theme": "system"}}
	return store
}

func BenchmarkDiagnosticServer(b *testing.B) {
	for _, tc := range []struct {
		name  string
		store *memorySnapshots
	}{
		{"全部分类", fixture(true)},
		{"不含输入事件", fixture(false)},
		{"仅设置快照", diagnosticServerConfigFixture()},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkDiagnosticServer(b, tc.store) })
	}
}

func TestDiagnosticServerAllocationBudget(t *testing.T) {
	// SDK 注册仍需编码 schema、检查参数头注解；预算保留这些开销，约束重复的类型推导和解析。
	for _, tc := range []struct {
		name   string
		store  *memorySnapshots
		budget int64
	}{
		{"全部分类", fixture(true), 192 << 10},
		{"不含输入事件", fixture(false), 144 << 10},
		{"仅设置快照", diagnosticServerConfigFixture(), 48 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := testing.Benchmark(func(b *testing.B) { benchmarkDiagnosticServer(b, tc.store) })
			if allocated := result.AllocedBytesPerOp(); allocated > tc.budget {
				t.Fatalf("诊断 server 注册分配 %d B，超过 %d B 预算", allocated, tc.budget)
			}
		})
	}
}

func TestDiagnosticServersKeepSnapshotsIsolated(t *testing.T) {
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			withInput := i%2 == 0
			store := fixture(withInput)
			marker := fmt.Sprintf("synthetic-%d", i)
			store.snapshot.AppVersion = fmt.Sprintf("1.0.%d", i)
			store.snapshot.ExpiresAt = time.Date(2026, 10, 24, i, 0, 0, 0, time.UTC)
			store.snapshot.Sections.ConfigSnapshot["theme"] = marker
			(*store.snapshot.Sections.CrashLogs)[0].Message = marker
			(*store.snapshot.Sections.PerfTrace)[0].TMS = int64Pointer(int64(i))
			if withInput {
				(*store.snapshot.Sections.InputEvents)[0].TMS = int64Pointer(int64(i + 100))
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			t.Cleanup(cancel)
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverSession, err := NewServer(store, store.snapshot).Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { serverSession.Close() })
			client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-client", Version: "1"}, nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { session.Close() })
			instructions := session.InitializeResult().Instructions
			if !strings.Contains(instructions, store.snapshot.Platform+" "+store.snapshot.AppVersion) || !strings.Contains(instructions, store.snapshot.ExpiresAt.Format(time.RFC3339)) {
				t.Fatal("server 说明未使用当前快照的元数据")
			}
			wantNames := []string{"get_config_snapshot", "get_perf_trace", "read_crash_logs"}
			if withInput {
				wantNames = append(wantNames, "get_input_events")
				slices.Sort(wantNames)
			}
			if names := toolNames(t, session); !slices.Equal(names, wantNames) {
				t.Fatalf("工具分类串入其他快照：实际=%v，期望=%v", names, wantNames)
			}
			config := call(t, session, "get_config_snapshot", nil)
			if config["config"].(map[string]any)["theme"] != marker {
				t.Fatal("设置内容串入其他快照")
			}
			crashes := call(t, session, "read_crash_logs", map[string]any{"limit": 1})["crash_logs"].([]any)
			if len(crashes) != 1 || crashes[0].(map[string]any)["message"] != marker {
				t.Fatal("崩溃内容或限量规则串入其他快照")
			}
			perf := call(t, session, "get_perf_trace", map[string]any{"limit": 1})["events"].([]any)
			if len(perf) != 1 || perf[0].(map[string]any)["t_ms"] != float64(i) {
				t.Fatal("性能轨迹串入其他快照")
			}
			if withInput {
				input := call(t, session, "get_input_events", map[string]any{"limit": 1})["events"].([]any)
				if len(input) != 1 || input[0].(map[string]any)["t_ms"] != float64(i+100) {
					t.Fatal("输入事件串入其他快照")
				}
			}
			for _, invalid := range []struct {
				name string
				args map[string]any
			}{
				{"read_crash_logs", map[string]any{"limit": "synthetic-invalid"}},
				{"get_perf_trace", map[string]any{"kind": 1}},
				{"get_perf_trace", map[string]any{"limit": "synthetic-invalid"}},
			} {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: invalid.name, Arguments: invalid.args})
				if err != nil {
					t.Fatal(err)
				}
				if !result.IsError {
					t.Fatal("类型错误的工具参数未被 schema 拒绝")
				}
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.accesses) != len(wantNames) {
				t.Fatalf("审计记录串入其他存储：实际=%d，期望=%d", len(store.accesses), len(wantNames))
			}
		})
	}
}
