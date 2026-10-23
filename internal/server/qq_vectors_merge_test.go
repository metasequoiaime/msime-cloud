package server

import (
	"slices"
	"testing"
)

func TestQQMergeSectionsReusesPickedWhenNothingCanBeAdded(t *testing.T) {
	picked := []int{2, 5}
	for _, recalled := range [][]int{nil, {}, {2}} {
		got := qqMergeSections(picked, recalled, len(picked))
		if !slices.Equal(got, picked) {
			t.Fatalf("无新增段落时结果变化：得到=%v，期望=%v", got, picked)
		}
		if len(got) > 0 && &got[0] != &picked[0] {
			t.Fatalf("无新增段落时复制了已选下标：得到=%p，期望=%p", &got[0], &picked[0])
		}
	}
}

func TestQQMergeSectionsCopiesBeforeAppending(t *testing.T) {
	picked := []int{2, 5}
	got := qqMergeSections(picked, []int{5, 8}, 3)
	if want := []int{2, 5, 8}; !slices.Equal(got, want) {
		t.Fatalf("追加召回段落错误：得到=%v，期望=%v", got, want)
	}
	if want := []int{2, 5}; !slices.Equal(picked, want) {
		t.Fatalf("追加召回段落修改了已选下标：得到=%v，期望=%v", picked, want)
	}
}

func BenchmarkQQMergeSections(b *testing.B) {
	picked := []int{2, 5}
	b.Run("no_recall", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if got := qqMergeSections(picked, nil, 8); len(got) != len(picked) {
				b.Fatal("已选段落数量变化")
			}
		}
	})
	b.Run("append", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if got := qqMergeSections(picked, []int{8}, 8); len(got) != len(picked)+1 {
				b.Fatal("召回段落未追加")
			}
		}
	})
}
