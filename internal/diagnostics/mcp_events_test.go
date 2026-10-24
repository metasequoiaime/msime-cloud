package diagnostics

import (
	"reflect"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

var diagnosticEventsBenchmarkSink []Event

func diagnosticEventFixture() []account.DiagnosticEvent {
	timestamps := make([]int64, 8192)
	source := make([]account.DiagnosticEvent, len(timestamps))
	for i := range source {
		timestamps[i] = int64(i)
		kind := "key_down"
		if i%2 == 1 {
			kind = "commit"
		}
		source[i] = account.DiagnosticEvent{TMS: &timestamps[i], Kind: kind}
	}
	source[len(source)-1].Kind = "candidate_shown"
	return source
}

func benchmarkDiagnosticEvents(b *testing.B, source []account.DiagnosticEvent, in EventInput, want int) {
	b.ReportAllocs()
	b.Cleanup(func() { diagnosticEventsBenchmarkSink = nil })
	for b.Loop() {
		out := events(source, in)
		if out == nil || len(out) != want {
			b.Fatalf("诊断事件条数=%d，期望=%d，空值=%v", len(out), want, out == nil)
		}
		diagnosticEventsBenchmarkSink = out
	}
}

func BenchmarkDiagnosticEvents(b *testing.B) {
	source := diagnosticEventFixture()
	for _, tc := range []struct {
		name string
		in   EventInput
		want int
	}{
		{"全部事件", EventInput{}, 8192},
		{"限制10条", EventInput{Limit: 10}, 10},
		{"限制1条", EventInput{Limit: 1}, 1},
		{"类型筛选", EventInput{Kind: "commit"}, 4095},
		{"单条筛选", EventInput{Kind: "candidate_shown"}, 1},
		{"无匹配", EventInput{Kind: "candidate_selected"}, 0},
		{"筛选并限制", EventInput{Kind: "commit", Limit: 10}, 10},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkDiagnosticEvents(b, source, tc.in, tc.want)
		})
	}
}

func TestDiagnosticEventsAllocationBudget(t *testing.T) {
	source := diagnosticEventFixture()
	t.Cleanup(func() { diagnosticEventsBenchmarkSink = nil })
	for _, tc := range []struct {
		name        string
		in          EventInput
		want        int
		budget      int64
		allocations float64
	}{
		{"全部事件", EventInput{}, 8192, 300 * 1024, 1},
		{"限制10条", EventInput{Limit: 10}, 10, 512, 1},
		{"单条筛选", EventInput{Kind: "candidate_shown"}, 1, 64, 1},
		{"无匹配", EventInput{Kind: "candidate_selected"}, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := events(source, tc.in); out == nil || len(out) != tc.want {
				t.Fatal("诊断事件输出条数不正确")
			}
			// 固定次数测量另外约束重复扩容，避免短时基准被少量其他分配影响。
			allocations := testing.AllocsPerRun(100, func() {
				diagnosticEventsBenchmarkSink = events(source, tc.in)
			})
			if allocations > tc.allocations {
				t.Fatalf("诊断事件投影分配 %.0f 次，超过 %.0f 次预算", allocations, tc.allocations)
			}
			// 基准读取进程级分配总量；其他协程只会增加字节数，取三次采样的最小值降低这类干扰。
			var allocated int64
			for sample := range 3 {
				result := testing.Benchmark(func(b *testing.B) {
					benchmarkDiagnosticEvents(b, source, tc.in, tc.want)
				})
				if sample == 0 || result.AllocedBytesPerOp() < allocated {
					allocated = result.AllocedBytesPerOp()
				}
			}
			if allocated > tc.budget {
				t.Fatalf("诊断事件投影分配 %d B，超过 %d B 预算", allocated, tc.budget)
			}
		})
	}
}

func TestDiagnosticEventsProjection(t *testing.T) {
	duration := int64Pointer(3)
	source := []account.DiagnosticEvent{
		{TMS: int64Pointer(0), Kind: "key_down"},
		{TMS: int64Pointer(2), Kind: "commit", DurationMS: duration},
		{TMS: int64Pointer(4), Kind: "commit"},
	}
	want := []Event{{TMS: 0, Kind: "key_down"}, {TMS: 2, Kind: "commit", DurationMS: duration}, {TMS: 4, Kind: "commit"}}
	maximum := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name   string
		source []account.DiagnosticEvent
		in     EventInput
		want   []Event
	}{
		{"空值列表", nil, EventInput{}, []Event{}},
		{"空列表", []account.DiagnosticEvent{}, EventInput{}, []Event{}},
		{"全部事件", source, EventInput{}, want},
		{"首条零时间戳", source, EventInput{Limit: 1}, want[:1]},
		{"限制超过列表", source, EventInput{Limit: 10}, want},
		{"最大限制", source, EventInput{Limit: maximum}, want},
		{"负数限制", source, EventInput{Limit: -1}, want},
		{"最小限制", source, EventInput{Limit: -maximum - 1}, want},
		{"类型筛选", source, EventInput{Kind: "commit"}, want[1:]},
		{"筛选后限制", source, EventInput{Kind: "commit", Limit: 1}, want[1:2]},
		{"筛选后负数限制", source, EventInput{Kind: "commit", Limit: -1}, want[1:]},
		{"筛选区分大小写", source, EventInput{Kind: "Commit"}, []Event{}},
		{"筛选无匹配", source, EventInput{Kind: "candidate_selected"}, []Event{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := events(tc.source, tc.in)
			if out == nil || !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("诊断事件投影变化：实际=%+v，期望=%+v，空值=%v", out, tc.want, out == nil)
			}
		})
	}
}
