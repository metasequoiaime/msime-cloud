package server

import (
	"reflect"
	"testing"
)

func TestCleanNativeCloudCandidatesEmptyAllocationBudget(t *testing.T) {
	allocations := testing.AllocsPerRun(100, func() {
		got := cleanNativeCloudCandidates(nil, 5)
		if got == nil || len(got) != 0 {
			t.Fatalf("空候选应返回非 nil 空数组：%v", got)
		}
	})
	if allocations != 0 {
		t.Fatalf("空候选过滤分配 %.1f 次，期望零分配", allocations)
	}
}

func TestCleanNativeCloudCandidatesInvalidAllocationBudget(t *testing.T) {
	allocations := testing.AllocsPerRun(100, func() {
		got := cleanNativeCloudCandidates([]nativeCloudCandidate{{Word: "hello"}, {Word: "你\n好"}}, 5)
		if got == nil || len(got) != 0 {
			t.Fatalf("全无效候选应返回非 nil 空数组：%v", got)
		}
	})
	if allocations != 0 {
		t.Fatalf("全无效候选过滤分配 %.1f 次，期望零分配", allocations)
	}
}

func TestCleanNativeCloudCandidatesFiltering(t *testing.T) {
	got := cleanNativeCloudCandidates([]nativeCloudCandidate{
		{Word: " "}, {Word: "hello"}, {Word: "你\n好"}, {Word: " 你好 "}, {Word: "你好"}, {Word: "您好"},
	}, 2)
	if want := []string{"你好", "您好"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("候选过滤发生变化：got=%v want=%v", got, want)
	}
	if got := cleanNativeCloudCandidates([]nativeCloudCandidate{{Word: "你好"}, {Word: "您好"}}, 1); !reflect.DeepEqual(got, []string{"你好"}) {
		t.Fatalf("候选上限发生变化：%v", got)
	}
}

func BenchmarkCleanNativeCloudCandidates(b *testing.B) {
	for _, tc := range []struct {
		name       string
		candidates []nativeCloudCandidate
		limit      int
	}{
		{"empty", nil, 5},
		{"invalid", []nativeCloudCandidate{{Word: "hello"}, {Word: "你\n好"}}, 5},
		{"valid", []nativeCloudCandidate{{Word: "你好"}, {Word: "您好"}}, 5},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = cleanNativeCloudCandidates(tc.candidates, tc.limit)
			}
		})
	}
}
