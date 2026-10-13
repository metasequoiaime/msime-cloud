package server

import (
	"strings"
	"testing"
)

func TestDictionaryTextPathAndRows(t *testing.T) {
	tests := map[string]bool{
		"custom/words.txt": true, "packs/programming/quanpin.txt": true, "sources/pinyin/rime-ice.txt": true,
		"../custom/words.txt": false, "custom/words.txt/next": false, "README.md": false, "custom/words.exe": false,
	}
	for name, want := range tests {
		if got := dictionaryTextPath(name); got != want {
			t.Errorf("dictionaryTextPath(%q)=%v, want %v", name, got, want)
		}
	}
	text := "# header\n未来可期\twei'lai'ke'qi\t5000\n第二行\ter\t1\n"
	rows, total := dictionaryRows("custom/words.txt", text, "未来", 1)
	if total != 1 || len(rows) != 1 || rows[0].Line != 2 || len(rows[0].Columns) != 3 {
		t.Fatalf("rows=%+v total=%d", rows, total)
	}
}

func TestApplyDictionaryChangesKeepsAppendRules(t *testing.T) {
	old := "# header\n甲\tjia\t1\n乙\tyi\t5\n"
	next, err := applyDictionaryChanges("custom/words.txt", old, []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}})
	if err != nil || !strings.HasSuffix(next, "丙\tbing\t3\n") {
		t.Fatalf("next=%q err=%v", next, err)
	}
	if _, err := applyDictionaryChanges("custom/words.txt", old, []dictionaryEdit{{Operation: "replace", Line: 2, Columns: []string{"甲", "jia", "5"}}}); err == nil {
		t.Fatal("custom replacement unexpectedly accepted")
	}
	if _, err := applyDictionaryChanges("packs/programming/quanpin.txt", old, []dictionaryEdit{{Operation: "replace", Line: 2, Columns: []string{"甲", "jia", "5"}}}); err != nil {
		t.Fatalf("pack replacement rejected: %v", err)
	}
}

func TestDictionaryBlobSHA(t *testing.T) {
	if got := dictionaryBlobSHA([]byte("hello")); got != "b6fc4c620b67d95f953a5c1c1230aaab5db5a1b0" {
		t.Fatalf("blob sha=%s", got)
	}
}

func TestDictionaryRowsFormatsAndPagination(t *testing.T) {
	var large strings.Builder
	large.WriteString("# 说明\n")
	for i := 0; i < 205; i++ {
		large.WriteString("example\tExample phrase\t1\n")
	}
	rows, total := dictionaryRows("custom/english.txt", large.String(), "EXAMPLE", 2)
	if total != 205 || len(rows) != 100 || rows[0].Line != 102 || rows[0].Columns[1] != "Example phrase" {
		t.Fatalf("page=%+v total=%d", rows, total)
	}
	rows, total = dictionaryRows("source.csv", "\"词,语\",100,\"注音\"\n", "", 1)
	if total != 1 || rows[0].Columns[0] != "词,语" {
		t.Fatalf("csv=%+v", rows)
	}
	rows, total = dictionaryRows("source.yaml", "---\nname: ignored\n...\n# 注释\n字\tzi\t1\n", "", 1)
	if total != 1 || rows[0].Line != 5 {
		t.Fatalf("yaml=%+v total=%d", rows, total)
	}
	rows, total = dictionaryRows("source.txt", "れい\t1\t2\t300\t例\n", "", 1)
	if total != 1 || len(rows[0].Columns) != 5 {
		t.Fatalf("japanese=%+v", rows)
	}
	rows, total = dictionaryRows("source.txt", "a\tb\n", "missing", 1)
	if total != 0 || len(rows) != 0 {
		t.Fatalf("empty=%+v total=%d", rows, total)
	}
}

func TestDecodeDictionaryText(t *testing.T) {
	for _, raw := range [][]byte{[]byte("词语"), {0xff, 0xfe, 0x57, 0x5b}, {0xfe, 0xff, 0x5b, 0x57}} {
		text, encoding, err := decodeDictionaryText(raw)
		if err != nil || text == "" || encoding == "" {
			t.Fatal(text, encoding, err)
		}
	}
	if _, _, err := decodeDictionaryText([]byte{0x80, 0x81}); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestApplyDictionaryChangesValidationAndBytes(t *testing.T) {
	old := "# 说明\r\n甲\tjia\t1\r\n乙\tyi\t5\r\n"
	got, err := applyDictionaryChanges("custom/words.txt", old, []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}})
	if err != nil || got != old+"丙\tbing\t3\r\n" {
		t.Fatalf("CRLF=%q err=%v", got, err)
	}
	got, err = applyDictionaryChanges("packs/demo/quanpin.txt", old, []dictionaryEdit{{Operation: "delete", Line: 2}, {Operation: "replace", Line: 3, Columns: []string{"乙", "yi", "10"}}})
	if err != nil || got != "# 说明\r\n乙\tyi\t10\r\n" {
		t.Fatalf("pack=%q err=%v", got, err)
	}
	for _, edit := range []dictionaryEdit{
		{Operation: "invalid"}, {Operation: "delete", Line: 1}, {Operation: "delete", Line: 30},
		{Operation: "add", Line: 2, Columns: []string{"丙", "bing", "3"}},
		{Operation: "add", Columns: []string{"甲", "jia", "3"}},
		{Operation: "add", Columns: []string{"丙", "bing", "99999"}},
		{Operation: "add", Columns: []string{"丙", "invalid", "3"}},
		{Operation: "add", Columns: []string{"丙\n", "bing", "3"}},
		{Operation: "add", Columns: []string{"丙", "bing"}},
	} {
		if _, err := applyDictionaryChanges("packs/demo/quanpin.txt", old, []dictionaryEdit{edit}); err == nil {
			t.Errorf("invalid edit accepted: %+v", edit)
		}
	}
	if _, err := applyDictionaryChanges("sources/pinyin/a.txt", old, []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "3"}}}); err == nil {
		t.Fatal("source write accepted")
	}
	if _, err := applyDictionaryChanges("custom/words.txt", old, []dictionaryEdit{{Operation: "add", Columns: []string{"丙", "bing", "10"}}}); err == nil {
		t.Fatal("weight outside existing range accepted")
	}
	if _, err := applyDictionaryChanges("packs/demo/translations.txt", "源词\t旧译文\n", []dictionaryEdit{{Operation: "add", Columns: []string{"源词", "new gloss with spaces"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyDictionaryChanges("packs/demo/english.txt", "example\tExample\t1\n", []dictionaryEdit{{Operation: "add", Columns: []string{"multi-word", "Multi-word", "10"}}}); err != nil {
		t.Fatal(err)
	}
}
