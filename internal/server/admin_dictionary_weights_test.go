package server

import (
	"errors"
	"fmt"
	"math/rand"
	"path"
	"strconv"
	"strings"
	"testing"
)

func previousValidateDictionaryColumns(name, existing string, cols []string) error {
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
		minimum, maximum := int64(0), int64(0)
		for _, line := range strings.Split(existing, "\n") {
			fields := strings.Split(strings.TrimSpace(line), "\t")
			if len(fields) != 3 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			n, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				continue
			}
			if minimum == 0 || n < minimum {
				minimum = n
			}
			if n > maximum {
				maximum = n
			}
		}
		if minimum == 0 || weight < minimum || weight > maximum {
			return invalid()
		}
	}
	return nil
}

func dictionaryWeightBatchFixture(rows, additions int) (string, []dictionaryEdit) {
	word := func(i int) string {
		return fmt.Sprintf("synthetic%c%c%c", 'a'+rune(i/(26*26)%26), 'a'+rune(i/26%26), 'a'+rune(i%26))
	}
	var text strings.Builder
	for i := range rows {
		fmt.Fprintf(&text, "%s\tSynthetic entry\t%d\n", word(i), i%5+1)
	}
	edits := make([]dictionaryEdit, additions)
	for i := range edits {
		edits[i] = dictionaryEdit{Operation: "add", Columns: []string{word(rows + i), "Synthetic addition", "3"}}
	}
	return text.String(), edits
}

func TestApplyDictionaryWeightBatchAllocationBudget(t *testing.T) {
	text, edits := dictionaryWeightBatchFixture(5000, 100)
	var got string
	var err error
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got, err = applyDictionaryChanges("custom/english.txt", text, edits)
		}
	})
	var want strings.Builder
	want.WriteString(text)
	for _, edit := range edits {
		want.WriteString(strings.Join(edit.Columns, "\t"))
		want.WriteByte('\n')
	}
	if err != nil || got != want.String() {
		t.Fatalf("批量追加结果变化：err=%v", err)
	}
	if bytes := result.AllocedBytesPerOp(); bytes > 8<<20 {
		t.Fatalf("五千行词库追加百条词条分配 %d 字节，超过 8 MiB 预算", bytes)
	}
}

func TestApplyDictionaryWeightValidationMatchesPrevious(t *testing.T) {
	texts := []string{
		"", "# 说明\n", "existing\tExisting\t1\nexistingb\tExisting B\t5\n",
		"existing\tExisting\t1\r\nexistingb\tExisting B\t5\r\n",
		"#ignored\tignored\t999\nexisting\tExisting\t3\n",
		"bad\nwrong\tcolumns\t3\textra\nexisting\tExisting\tnan\n",
		"existing\tExisting\t0\nexistingb\tExisting B\t5\n",
		"existing\tExisting\t-5\nexistingb\tExisting B\t5\n",
		"existing\tExisting\t9223372036854775808\nexistingb\tExisting B\t5\n",
		"existing\tExisting\t3\nexistingb\tExisting B\t0\nexistingc\tExisting C\t5",
	}
	names := []string{"custom/words.txt", "custom/english.txt", "custom/translations.txt", "packs/demo/quanpin.txt", "packs/demo/english.txt", "packs/demo/translations.txt"}
	for _, name := range names {
		for _, text := range texts {
			for _, weights := range [][]string{{"1", "5", "3"}, {"3", "6"}, {"3", "0"}, {"3", "10001"}, {"bad"}, {"9223372036854775808"}} {
				edits := make([]dictionaryEdit, len(weights))
				var want error
				for i, weight := range weights {
					cols := []string{[]string{"甲", "乙", "丙"}[i], []string{"jia", "yi", "bing"}[i], weight}
					if path.Base(name) == "english.txt" {
						cols = []string{fmt.Sprintf("synthetic%c", 'a'+rune(i)), "Synthetic", weight}
					}
					if path.Base(name) == "translations.txt" {
						cols = []string{fmt.Sprintf("源词%c", '甲'+rune(i)), "synthetic gloss"}
					}
					edits[i] = dictionaryEdit{Operation: "add", Columns: cols}
					if want == nil {
						want = previousValidateDictionaryColumns(name, text, cols)
					}
				}
				_, got := applyDictionaryChanges(name, text, edits)
				var gotChange, wantChange *dictionaryChangeError
				if (got == nil) != (want == nil) || (got != nil && (!errors.As(got, &gotChange) || !errors.As(want, &wantChange) || *gotChange != *wantChange)) {
					t.Fatalf("权重校验变化：文件=%q 原文=%q 权重=%v got=%v want=%v", name, text, weights, got, want)
				}
			}
		}
	}
}

func TestDictionaryWeightScanAllocationBudget(t *testing.T) {
	text, edits := dictionaryWeightBatchFixture(5000, 1)
	var err error
	allocations := testing.AllocsPerRun(3, func() {
		validator := dictionaryColumnValidator{name: "custom/english.txt", text: text}
		err = validator.validate(edits[0].Columns)
	})
	if err != nil {
		t.Fatalf("有效权重被拒绝：%v", err)
	}
	if allocations > 10 {
		t.Fatalf("扫描五千行权重分配 %.0f 次，超过 10 次预算", allocations)
	}
}

func TestDictionaryWeightFieldScanMatchesPrevious(t *testing.T) {
	texts := []string{
		"\tword\tdisplay\t3\t\r\n",
		"word\t\t3\n",
		"word\tdisplay\t3\textra\n",
		"\u2003word\tdisplay\t+5\u2003\n",
		"#ignored\tdisplay\t9\nword\tdisplay\t3\n",
		"word\tdisplay\t-5\nother\tdisplay\t0\nlast\tdisplay\t5\n",
	}
	random := rand.New(rand.NewSource(718))
	fields := []string{"word", "display", "", "#ignored", "0", "-5", "+5", "3", "nan", "9223372036854775808", "\r", "\u2003"}
	for range 500 {
		var text strings.Builder
		for range random.Intn(8) {
			for i := range random.Intn(6) {
				if i > 0 {
					text.WriteByte('\t')
				}
				text.WriteString(fields[random.Intn(len(fields))])
			}
			text.WriteByte('\n')
		}
		texts = append(texts, text.String())
	}
	for _, text := range texts {
		validator := dictionaryColumnValidator{name: "custom/english.txt", text: text}
		for _, weight := range []string{"1", "3", "5", "6", "10000"} {
			cols := []string{"synthetic", "Synthetic", weight}
			got := validator.validate(cols)
			want := previousValidateDictionaryColumns(validator.name, text, cols)
			var gotChange, wantChange *dictionaryChangeError
			if (got == nil) != (want == nil) || (got != nil && (!errors.As(got, &gotChange) || !errors.As(want, &wantChange) || *gotChange != *wantChange)) {
				t.Fatalf("权重字段扫描变化：原文=%q 权重=%q got=%v want=%v", text, weight, got, want)
			}
		}
	}
}

func BenchmarkApplyDictionaryWeightBatch(b *testing.B) {
	for _, rows := range []int{100, 5000} {
		for _, size := range []int{1, 100} {
			text, edits := dictionaryWeightBatchFixture(rows, size)
			b.Run(fmt.Sprintf("%d/%d", rows, size), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := applyDictionaryChanges("custom/english.txt", text, edits); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
