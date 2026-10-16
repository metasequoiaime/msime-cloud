package server

import (
	"reflect"
	"strings"
	"testing"
)

func previousTrimDictionaryContent(content string, drop map[int]bool) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestTrimDictionaryContentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		drop          map[int]bool
	}{
		{"空文件", "", nil},
		{"删除首行", "首\n中\n尾", map[int]bool{0: true}},
		{"删除中间行", "首\n中\n尾", map[int]bool{1: true}},
		{"删除末尾换行空段", "首\n中\n", map[int]bool{2: true}},
		{"CRLF按原文本保留", "首\r\n删\r\n尾\r\n", map[int]bool{1: true}},
		{"超长行", strings.Repeat("合成", 100000), map[int]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := previousTrimDictionaryContent(tc.content, tc.drop)
			got := trimDictionaryContent(tc.content, tc.drop)
			if got != want {
				t.Fatalf("结果=%q，期望=%q", got, want)
			}
		})
	}
}

func TestTrimDictionaryContentMatchesPreviousRandom(t *testing.T) {
	for _, tc := range []struct {
		content string
		drop    map[int]bool
	}{
		{"", nil},
		{"\n", map[int]bool{0: true}},
		{"甲\n\n乙\n# 注释\n", map[int]bool{1: true, 3: true}},
		{"\xff\n\xe4\xb8\n尾", map[int]bool{0: true, 2: true}},
	} {
		if got, want := trimDictionaryContent(tc.content, tc.drop), previousTrimDictionaryContent(tc.content, tc.drop); got != want {
			t.Fatalf("结果=%q，期望=%q", got, want)
		}
	}
}

func TestTrimDictionaryContentAllocationBudget(t *testing.T) {
	content := strings.Repeat("合成词条\tci'tiao\t1\n", 50000)
	drop := map[int]bool{0: true}
	var got string
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = trimDictionaryContent(content, drop)
		}
	})
	if got == "" || len(got) >= len(content) {
		t.Fatal("删除内容未生效")
	}
	old := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = previousTrimDictionaryContent(content, drop)
		}
	})
	t.Logf("新：%s，旧：%s", result.MemString(), old.MemString())
	if result.AllocedBytesPerOp() >= old.AllocedBytesPerOp() {
		t.Fatalf("新实现分配 %d 字节，不少于旧实现 %d 字节", result.AllocedBytesPerOp(), old.AllocedBytesPerOp())
	}
}

func TestTrimDictionaryContentResultIsIndependent(t *testing.T) {
	content := "首\n删\n尾"
	drop := map[int]bool{1: true}
	if !reflect.DeepEqual(trimDictionaryContent(content, drop), "首\n尾") {
		t.Fatal("删除结果不符合预期")
	}
}
