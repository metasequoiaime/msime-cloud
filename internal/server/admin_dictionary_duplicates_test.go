package server

import (
	"errors"
	"strings"
	"testing"
)

func TestApplyDictionaryDuplicateAllowance(t *testing.T) {
	add := func(word, display string) dictionaryEdit {
		return dictionaryEdit{Operation: "add", Columns: []string{word, display, "3"}}
	}
	for _, tc := range []struct {
		name    string
		text    string
		edits   []dictionaryEdit
		want    string
		wantErr string
	}{
		{"保留原有重复", "old\tOld\t1\nold\tOld\t2\n", []dictionaryEdit{add("new", "New")}, "old\tOld\t1\nold\tOld\t2\nnew\tNew\t3\n", ""},
		{"原有重复不允许增加", "old\tOld\t1\nold\tOld\t2\n", []dictionaryEdit{add("old", "Old")}, "", "dictionary_duplicate"},
		{"删除后补回原有重复", "old\tOld\t1\nold\tOld\t2\n", []dictionaryEdit{{Operation: "delete", Line: 1}, add("old", "Old")}, "old\tOld\t2\nold\tOld\t3\n", ""},
		{"删除全部后补回重复", "old\tOld\t1\nold\tOld\t2\n", []dictionaryEdit{{Operation: "delete", Line: 1}, {Operation: "delete", Line: 2}, add("old", "Old"), add("old", "Old")}, "old\tOld\t3\nold\tOld\t3\n", ""},
		{"新词只允许一次", "# 说明\n无列\n\n", []dictionaryEdit{add("new", "New"), add("new", "New")}, "", "dictionary_duplicate"},
		{"相同首列不同次列", "old\tOld\t1\n", []dictionaryEdit{add("old", "Other")}, "old\tOld\t1\nold\tOther\t3\n", ""},
		{"替换不能增加原有重复", "old\tOld\t1\nother\tOther\t2\n", []dictionaryEdit{{Operation: "replace", Line: 2, Columns: []string{"old", "Old", "3"}}}, "", "dictionary_duplicate"},
		{"换行和空列原样保留", "\t\r\n\t\r\nold\tOld\t1\r\n", []dictionaryEdit{add("new", "New")}, "\t\r\n\t\r\nold\tOld\t1\r\nnew\tNew\t3\r\n", ""},
		{"优先返回词条校验错误", "old\tOld\t1\n", []dictionaryEdit{add("old", "Old"), add("invalid1", "Invalid")}, "", "invalid_dictionary_entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyDictionaryChanges("packs/demo/english.txt", tc.text, tc.edits)
			var rejected *dictionaryChangeError
			if tc.wantErr != "" {
				if got != "" || !errors.As(err, &rejected) || rejected.code != tc.wantErr || rejected.status != map[string]int{"dictionary_duplicate": 409, "invalid_dictionary_entry": 400}[tc.wantErr] {
					t.Fatalf("got=%q err=%v，期望错误 %s", got, err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestApplyDictionaryTranslationDuplicates(t *testing.T) {
	text := "源词\t旧译文\n源词\t旧译文\n"
	for _, tc := range []struct {
		gloss string
		ok    bool
	}{{"新译文", true}, {"旧译文", false}} {
		edits := []dictionaryEdit{{Operation: "add", Columns: []string{"源词", tc.gloss}}}
		got, err := applyDictionaryChanges("packs/demo/translations.txt", text, edits)
		if tc.ok {
			if err != nil || got != text+"源词\t"+tc.gloss+"\n" {
				t.Fatalf("不同译文追加失败：got=%q err=%v", got, err)
			}
		} else {
			var rejected *dictionaryChangeError
			if got != "" || !errors.As(err, &rejected) || rejected.code != "dictionary_duplicate" || rejected.status != 409 {
				t.Fatalf("重复译文未被拒绝：got=%q err=%v", got, err)
			}
		}
	}
}

func TestApplyDictionaryDuplicateMemoryBudget(t *testing.T) {
	text, edits := dictionaryWeightBatchFixture(5000, 1)
	var got string
	var err error
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got, err = applyDictionaryChanges("packs/demo/english.txt", text, edits)
		}
	})
	if err != nil || got != text+strings.Join(edits[0].Columns, "\t")+"\n" {
		t.Fatalf("词库追加结果发生变化：err=%v", err)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 700<<10 {
		t.Fatalf("五千行词库追加一条词条分配 %d 字节，超过 700 KiB 预算", bytes)
	}
}

func dictionaryEarlyDuplicateFixture() (string, []dictionaryEdit) {
	text := "alpha\tAlpha\t1\nbeta\tBeta\t2\n" + strings.Repeat("#\n", 100000)
	edits := []dictionaryEdit{{Operation: "replace", Line: 1, Columns: []string{"beta", "Beta", "3"}}}
	return text, edits
}

func TestApplyDictionaryEarlyDuplicateMemoryBudget(t *testing.T) {
	text, edits := dictionaryEarlyDuplicateFixture()
	var got string
	var err error
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got, err = applyDictionaryChanges("packs/demo/english.txt", text, edits)
		}
	})
	var rejected *dictionaryChangeError
	if got != "" || !errors.As(err, &rejected) || rejected.code != "dictionary_duplicate" || rejected.status != 409 {
		t.Fatalf("未拒绝开头的重复词条：got=%q err=%v", got, err)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 64<<10 {
		t.Fatalf("拒绝开头重复词条分配 %d 字节，超过 64 KiB 预算", bytes)
	}
}

func BenchmarkApplyDictionaryEarlyDuplicate(b *testing.B) {
	text, edits := dictionaryEarlyDuplicateFixture()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := applyDictionaryChanges("packs/demo/english.txt", text, edits); err == nil {
			b.Fatal("未拒绝重复词条")
		}
	}
}
