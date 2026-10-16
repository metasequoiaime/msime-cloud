package server

import (
	"context"
	"testing"
)

func TestCheckDictEntriesWithoutAdditionsAllocations(t *testing.T) {
	content := dictPRUnchangedContent()
	for _, tc := range []struct {
		name  string
		head  string
		added []int
	}{
		{"未变化文件", content, nil},
		{"空新增列表", content, []int{}},
		{"全部删除", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := make([]dictFile, 0, len(submissionKinds))
			for _, kind := range submissionKinds {
				files = append(files, dictFile{kind: kind, base: content, head: tc.head, added: tc.added})
			}
			s := &Server{}
			ctx := context.Background()
			var got []dictEntry
			allocations := testing.AllocsPerRun(3, func() { got = s.checkDictEntries(ctx, files) })
			if got == nil || len(got) != 0 {
				t.Fatalf("没有新增时应返回非 nil 空词条列表，得到 %#v", got)
			}
			if allocations != 0 {
				t.Fatalf("没有新增时分配 %g 次，期望零分配", allocations)
			}
		})
	}
}

func BenchmarkCheckDictEntriesWithoutAdditions(b *testing.B) {
	content := dictPRUnchangedContent()
	files := make([]dictFile, 0, len(submissionKinds))
	for _, kind := range submissionKinds {
		files = append(files, dictFile{kind: kind, base: content, head: content})
	}
	s := &Server{}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if got := s.checkDictEntries(ctx, files); got == nil || len(got) != 0 {
			b.Fatal("没有新增时应返回非 nil 空词条列表")
		}
	}
}
