package server

import (
	"strings"
	"testing"
)

func TestApplyDictionaryOutputBytes(t *testing.T) {
	add := dictionaryEdit{Operation: "add", Columns: []string{"new", "新词", "3"}}
	replace := dictionaryEdit{Operation: "replace", Line: 1, Columns: []string{"new", "新词", "3"}}
	remove := dictionaryEdit{Operation: "delete", Line: 1}
	for _, tc := range []struct {
		name  string
		text  string
		edits []dictionaryEdit
		want  string
	}{
		{"空文件追加", "", []dictionaryEdit{add}, "new\t新词\t3\n"},
		{"缺少末尾换行", "old\t旧词\t1", []dictionaryEdit{add}, "old\t旧词\t1\nnew\t新词\t3\n"},
		{"混合换行保留", "# 说明\r\nold\t旧词\t1\nlast\t末词\t2", []dictionaryEdit{add}, "# 说明\r\nold\t旧词\t1\nlast\t末词\t2\r\nnew\t新词\t3\r\n"},
		{"连续追加保留空行", "old\t旧词\t1\n\n", []dictionaryEdit{add, {Operation: "add", Columns: []string{"other", "其他", "2"}}}, "old\t旧词\t1\n\nnew\t新词\t3\nother\t其他\t2\n"},
		{"替换末尾行", "old\t旧词\t1", []dictionaryEdit{replace}, "new\t新词\t3"},
		{"替换为更长行", "a\tA\t1", []dictionaryEdit{replace}, "new\t新词\t3"},
		{"替换为更短行", "longword\t原来的长词条\t1\r\n", []dictionaryEdit{replace}, "new\t新词\t3\r\n"},
		{"替换保留回车", "old\t旧词\t1\r\n", []dictionaryEdit{replace}, "new\t新词\t3\r\n"},
		{"替换重复末尾回车", "old\t旧词\t1\r\r", []dictionaryEdit{replace}, "new\t新词\t3\r"},
		{"删除全部无末尾换行", "old\t旧词\t1", []dictionaryEdit{remove}, ""},
		{"删除全部有末尾换行", "old\t旧词\t1\r\n", []dictionaryEdit{remove}, ""},
		{"删除后追加", "old\t旧词\t1\r\n", []dictionaryEdit{remove, add}, "new\t新词\t3\r\n"},
		{"删除末尾行后追加", "old\t旧词\t1\nlast\t末词\t2", []dictionaryEdit{{Operation: "delete", Line: 2}, add}, "old\t旧词\t1\nnew\t新词\t3\n"},
		{"未变更字节保留", "# 说明\r\n\r\nold\t旧词\t1\r", nil, "# 说明\r\n\r\nold\t旧词\t1\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyDictionaryChanges("packs/demo/english.txt", tc.text, tc.edits)
			if err != nil || got != tc.want {
				t.Fatalf("got=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestApplyDictionaryOutputMemoryBudget(t *testing.T) {
	text, edits := dictionaryWeightBatchFixture(5000, 1)
	deletedText := strings.Repeat("synthetic\t"+strings.Repeat("x", 900)+"\t1\n", 100)
	deletions := make([]dictionaryEdit, 100)
	for i := range deletions {
		deletions[i] = dictionaryEdit{Operation: "delete", Line: i + 1}
	}
	for _, tc := range []struct {
		name   string
		text   string
		edits  []dictionaryEdit
		want   string
		budget int64
	}{
		{"追加五千行词库", text, edits, text + strings.Join(edits[0].Columns, "\t") + "\n", 1 << 20},
		{"删除全部词条", deletedText, deletions, "", 32 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var err error
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, err = applyDictionaryChanges("packs/demo/english.txt", tc.text, tc.edits)
				}
			})
			if err != nil || got != tc.want {
				t.Fatalf("词库输出发生变化：err=%v", err)
			}
			if bytes := result.AllocedBytesPerOp(); bytes > tc.budget {
				t.Fatalf("分配 %d 字节，超过 %d 字节预算", bytes, tc.budget)
			}
		})
	}
}
