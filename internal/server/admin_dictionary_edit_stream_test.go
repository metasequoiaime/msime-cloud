package server

import (
	"errors"
	"math/rand"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// previousApplyDictionaryChanges 保留原有行切片流程，作为流式处理的逐字节对照。
func previousApplyDictionaryChanges(name, text string, changes []dictionaryEdit) (string, error) {
	if !dictionaryEditablePath(name) || !utf8.ValidString(text) {
		return "", dictionaryChangeFail(400, "dictionary_readonly")
	}
	lines := strings.Split(text, "\n")
	validator := previousDictionaryColumnValidator{name: name, lines: lines}
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	updates := map[int]*string{}
	additions := []string{}
	for _, edit := range changes {
		if !slices.Contains([]string{"add", "replace", "delete"}, edit.Operation) || (edit.Operation != "add" && strings.HasPrefix(name, "custom/")) {
			return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
		}
		if edit.Operation == "add" {
			if edit.Line != 0 {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
		} else {
			if edit.Line < 1 || edit.Line > len(lines) || strings.TrimSpace(lines[edit.Line-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[edit.Line-1]), "#") {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
			if _, exists := updates[edit.Line]; exists {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
		}
		if edit.Operation == "delete" {
			if len(edit.Columns) != 0 {
				return "", dictionaryChangeFail(400, "invalid_dictionary_changes")
			}
			updates[edit.Line] = nil
			continue
		}
		if err := validator.validate(edit.Columns); err != nil {
			return "", err
		}
		value := strings.Join(edit.Columns, "\t")
		if edit.Operation == "add" {
			additions = append(additions, value)
		} else {
			updates[edit.Line] = &value
		}
	}
	// 按删改后的字节数预留空间，追加前可能需要补一个换行。
	outputBytes := len(text)
	for line, value := range updates {
		original := lines[line-1]
		if value == nil {
			outputBytes -= len(original)
			if line < len(lines) {
				outputBytes--
			}
		} else {
			outputBytes += len(*value) - len(original)
			if strings.HasSuffix(original, "\r") {
				outputBytes++
			}
		}
	}
	if outputBytes > 0 && len(additions) > 0 {
		outputBytes += len(newline)
	}
	for _, value := range additions {
		outputBytes += len(value) + len(newline)
	}
	// 保留未修改行的每一个字节及换行风格，仅修改目标行。
	var out strings.Builder
	out.Grow(outputBytes)
	for i, original := range lines {
		value, changed := updates[i+1]
		if changed && value == nil {
			continue
		}
		if changed {
			out.WriteString(*value)
			if strings.HasSuffix(original, "\r") {
				out.WriteByte('\r')
			}
		} else {
			out.WriteString(original)
		}
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	if len(additions) > 0 {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString(newline)
		}
		for _, value := range additions {
			out.WriteString(value)
			out.WriteString(newline)
		}
	}
	result := out.String()
	// 翻译允许覆盖同一源词，其他文件按前两列去重。
	remaining := map[string]int{}

	for _, line := range lines {
		if k := dictionaryEntryKey(line); k != "" {
			remaining[k]++
		}
	}
	for line := range strings.SplitSeq(result, "\n") {
		k := dictionaryEntryKey(line)
		if k == "" {
			continue
		}
		if count, exists := remaining[k]; !exists {
			// 新键只允许出现一次；保留零值以拒绝后续重复。
			remaining[k] = 0
		} else if count > 0 {
			remaining[k] = count - 1
		} else {
			return "", dictionaryChangeFail(409, "dictionary_duplicate")
		}
	}
	return result, nil
}

func TestApplyDictionaryStreamingMatchesPrevious(t *testing.T) {
	texts := []string{
		"", "\n", "\r\n", "# 合成说明\n\n", "synthetic\t合成条目\t3", "synthetic\t合成条目\t3\r\n",
		"first\t合成首行\t1\r\nlast\t合成末行\t5\r", "same\t合成条目\t1\nsame\t合成条目\t1\n",
		"synthetic\t合成条目\t3\r\r", "\xff\nsynthetic\t合成条目\t3\n",
	}
	random := rand.New(rand.NewSource(649))
	units := []string{"synthetic\t合成条目\t3", "first\t合成首行\t1", "last\t合成末行\t5", "甲\tjia\t3", "源词\t合成译文", "# 说明", "", " \t", "\r", "\x00", "bad\tfields\textra\t1"}
	for range 500 {
		var text strings.Builder
		for range random.Intn(12) {
			text.WriteString(units[random.Intn(len(units))])
			text.WriteString([]string{"", "\n", "\r\n"}[random.Intn(3)])
		}
		texts = append(texts, text.String())
	}
	columns := [][]string{
		{"new", "合成新词", "3"}, {"new", "合成新词", "6"}, {"甲", "jia", "3"}, {"源词", "合成译文"},
		nil, {"bad\nword", "合成新词", "3"}, {"new", "合成新词", "bad"}, {"new", "合成新词", "0"},
	}
	for _, text := range texts {
		lineCount := strings.Count(text, "\n") + 1
		batches := [][]dictionaryEdit{
			nil,
			{{Operation: "add", Columns: columns[0]}},
			{{Operation: "replace", Line: 1, Columns: columns[0]}},
			{{Operation: "delete", Line: lineCount}},
			{{Operation: "delete", Line: 1}, {Operation: "add", Columns: columns[0]}},
			{{Operation: "replace", Line: 1, Columns: columns[0]}, {Operation: "delete", Line: 1}},
		}
		for range 5 {
			edits := make([]dictionaryEdit, random.Intn(5)+1)
			for i := range edits {
				edits[i] = dictionaryEdit{
					Operation: []string{"add", "replace", "delete", "invalid"}[random.Intn(4)],
					Line:      random.Intn(lineCount+5) - 2,
					Columns:   columns[random.Intn(len(columns))],
				}
			}
			batches = append(batches, edits)
		}
		for _, name := range []string{"packs/demo/english.txt", "packs/demo/quanpin.txt", "packs/demo/translations.txt", "custom/english.txt", "custom/words.txt", "custom/translations.txt", "invalid.txt"} {
			for _, edits := range batches {
				got, gotErr := applyDictionaryChanges(name, text, edits)
				want, wantErr := previousApplyDictionaryChanges(name, text, edits)
				var gotChange, wantChange *dictionaryChangeError
				if got != want || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && (!errors.As(gotErr, &gotChange) || !errors.As(wantErr, &wantChange) || *gotChange != *wantChange)) {
					t.Fatalf("文件=%q，原文=%q，改动=%+v，结果=%q/%v，期望=%q/%v", name, text, edits, got, gotErr, want, wantErr)
				}
			}
		}
	}
}

func TestApplyDictionaryStreamingEditOrder(t *testing.T) {
	text := "first\t合成首行\t1\r\nsecond\t合成次行\t2\nthird\t合成三行\t3\r\nfourth\t合成尾行\t4"
	for _, tc := range []struct {
		name  string
		text  string
		edits []dictionaryEdit
		want  string
	}{
		{
			"乱序相邻删改后追加", text,
			[]dictionaryEdit{
				{Operation: "replace", Line: 4, Columns: []string{"last", "合成末行", "4"}},
				{Operation: "delete", Line: 2},
				{Operation: "replace", Line: 1, Columns: []string{"new", "合成新行", "1"}},
				{Operation: "delete", Line: 3},
				{Operation: "add", Columns: []string{"added", "合成追加", "3"}},
			},
			"new\t合成新行\t1\r\nlast\t合成末行\t4\r\nadded\t合成追加\t3\r\n",
		},
		{
			"删首尾保留中间换行", text,
			[]dictionaryEdit{
				{Operation: "delete", Line: 4},
				{Operation: "delete", Line: 1},
				{Operation: "replace", Line: 2, Columns: []string{"other", "合成其他", "2"}},
			},
			"other\t合成其他\t2\nthird\t合成三行\t3\r\n",
		},
		{
			"逆序删除百行", strings.Repeat("synthetic\t合成条目\t3\n", 100),
			func() []dictionaryEdit {
				edits := make([]dictionaryEdit, 100)
				for i := range edits {
					edits[i] = dictionaryEdit{Operation: "delete", Line: 100 - i}
				}
				return edits
			}(),
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyDictionaryChanges("packs/demo/english.txt", tc.text, tc.edits)
			if err != nil || got != tc.want {
				t.Fatalf("结果=%q/%v，期望=%q", got, err, tc.want)
			}
		})
	}
}

func dictionaryStreamingCases() []struct {
	name, path string
	edits      []dictionaryEdit
} {
	return []struct {
		name, path string
		edits      []dictionaryEdit
	}{
		{"内置追加", "packs/demo/english.txt", []dictionaryEdit{{Operation: "add", Columns: []string{"new", "合成新词", "3"}}}},
		{"自定义追加", "custom/english.txt", []dictionaryEdit{{Operation: "add", Columns: []string{"new", "合成新词", "3"}}}},
		{"替换末行", "packs/demo/english.txt", []dictionaryEdit{{Operation: "replace", Line: 50000, Columns: []string{"new", "合成新词", "3"}}}},
		{"删除末行", "packs/demo/english.txt", []dictionaryEdit{{Operation: "delete", Line: 50000}}},
		{"拒绝非法操作", "packs/demo/english.txt", []dictionaryEdit{{Operation: "invalid"}}},
	}
}

func TestApplyDictionaryStreamingAllocationBudget(t *testing.T) {
	text := strings.Repeat("synthetic\t合成条目\t3\n", 50000)
	for _, tc := range dictionaryStreamingCases() {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := previousApplyDictionaryChanges(tc.path, text, tc.edits)
			var got string
			var err error
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, err = applyDictionaryChanges(tc.path, text, tc.edits)
				}
			})
			budget := int64(len(text)) + (128 << 10)
			if wantErr != nil {
				budget = 8192
			}
			if got != want || (err == nil) != (wantErr == nil) || result.AllocedBytesPerOp() > budget {
				t.Fatalf("输出一致=%t，错误=%v/%v，分配=%s，预算=%d 字节", got == want, err, wantErr, result.MemString(), budget)
			}
		})
	}
}

func BenchmarkApplyDictionaryStreaming(b *testing.B) {
	text := strings.Repeat("synthetic\t合成条目\t3\n", 50000)
	for _, implementation := range []struct {
		name  string
		apply func(string, string, []dictionaryEdit) (string, error)
	}{
		{"previous", previousApplyDictionaryChanges},
		{"current", applyDictionaryChanges},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			for _, tc := range dictionaryStreamingCases() {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_, _ = implementation.apply(tc.path, text, tc.edits)
					}
				})
			}
		})
	}
}

type previousDictionaryColumnValidator struct {
	name        string
	lines       []string
	weightsRead bool
	minimum     int64
	maximum     int64
}

func (v *previousDictionaryColumnValidator) validate(cols []string) error {
	name := v.name
	invalid := func() error { return dictionaryChangeFail(400, "invalid_dictionary_entry") }
	isTranslation := path.Base(name) == "translations.txt"
	required := 3
	if isTranslation {
		required = 2
	}
	if len(cols) != required {
		return invalid()
	}
	for _, field := range cols {
		if field == "" || strings.TrimSpace(field) != field || !plainText(field) || len(field) > 1000 || strings.HasPrefix(field, "#") {
			return invalid()
		}
	}
	if isTranslation {
		if len(validateTranslationEntries([]translationSubmissionEntry{{Source: cols[0], Gloss: cols[1]}})) > 0 {
			return invalid()
		}
		return nil
	}
	weight, err := strconv.ParseInt(cols[2], 10, 64)
	if err != nil || weight < 1 {
		return invalid()
	}
	if path.Base(name) == "english.txt" {
		if strings.HasPrefix(name, "packs/") {
			if len(cols[0]) > 64 || strings.IndexFunc(cols[0], func(r rune) bool { return !(r >= 'a' && r <= 'z' || r == '-' || r == '\'') }) >= 0 || weight > 10 {
				return invalid()
			}
		} else if len(validateEnglishEntries([]englishSubmissionEntry{{Word: cols[0], Display: cols[1]}})) > 0 {
			return invalid()
		}
	} else if len(validateWordEntries([]wordSubmissionEntry{{Word: cols[0], Pinyin: cols[1]}})) > 0 || weight > 10000 {
		return invalid()
	}
	if strings.HasPrefix(name, "custom/") {
		if !v.weightsRead {
			for _, line := range v.lines {
				first, rest, ok := strings.Cut(strings.TrimSpace(line), "\t")
				if !ok || strings.HasPrefix(first, "#") {
					continue
				}
				_, weightField, ok := strings.Cut(rest, "\t")
				if !ok || strings.Contains(weightField, "\t") {
					continue
				}
				n, err := strconv.ParseInt(weightField, 10, 64)
				if err != nil {
					continue
				}
				if v.minimum == 0 || n < v.minimum {
					v.minimum = n
				}
				if n > v.maximum {
					v.maximum = n
				}
			}
			v.weightsRead = true
		}
		if v.minimum == 0 || weight < v.minimum || weight > v.maximum {
			return invalid()
		}
	}
	return nil
}
