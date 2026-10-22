package server

import (
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// previousTencentTranslationBatches 保留优化前的批次构建，用于差分测试和分配比较。
func previousTencentTranslationBatches(texts []string) ([]translationPart, []tencentTranslationBatch) {
	parts := translationParts(texts, tencentBatchCharacters)
	var batches []tencentTranslationBatch
	batch, units := tencentTranslationBatch{}, 0
	for index, part := range parts {
		if part.text == "" {
			continue
		}
		// 参考计数独立于公共实现，并保留旧批次构建的零分配计数行为。
		count := 0
		for _, r := range part.text {
			count += utf16.RuneLen(r)
		}
		if units+count > tencentBatchCharacters && len(batch.texts) > 0 {
			batches = append(batches, batch)
			batch, units = tencentTranslationBatch{}, 0
		}
		batch.texts = append(batch.texts, part.text)
		batch.parts = append(batch.parts, index)
		units += count
	}
	if len(batch.texts) > 0 {
		batches = append(batches, batch)
	}
	return parts, batches
}

func tencentBatchBufferInputs() []struct {
	name  string
	texts []string
} {
	cases := []struct {
		name  string
		texts []string
	}{
		{"nil", nil},
		{"empty", []string{"", ""}},
		{"whitespace", []string{strings.Repeat(" ", 8192)}},
		{"ascii_limit", []string{strings.Repeat("a", 5999)}},
		{"ascii_request", []string{strings.Repeat("a", 8192)}},
		{"chinese", []string{strings.Repeat("中", 2500), strings.Repeat("文", 2500), strings.Repeat("字", 2500)}},
		{"emoji", []string{strings.Repeat("😀", 2000), strings.Repeat("😁", 2000)}},
		{"paragraphs", []string{strings.Repeat("word ", 1600)}},
		{"sparse", []string{"", strings.Repeat(" ", 8192), "word", strings.Repeat(" ", 8192), ""}},
		{"adjacent", []string{"a", "b", strings.Repeat("c", 5997), "d", "e"}},
	}
	for _, count := range []int{1, 9, translationBatchLimit} {
		texts := make([]string, count)
		for i := range texts {
			texts[i] = "synthetic-word-" + strconv.Itoa(i)
		}
		cases = append(cases, struct {
			name  string
			texts []string
		}{"words_" + strconv.Itoa(count), texts})
	}
	return cases
}

func checkTencentBatchBufferCompatibility(t *testing.T, texts []string) {
	t.Helper()
	before := slices.Clone(texts)
	gotParts, gotBatches := tencentTranslationBatches(texts)
	wantParts, wantBatches := previousTencentTranslationBatches(texts)
	if !reflect.DeepEqual(gotParts, wantParts) || !reflect.DeepEqual(gotBatches, wantBatches) {
		t.Fatal("分段、批次边界或原文下标与旧实现不一致")
	}
	// 每个批次的追加和元素修改都不能影响其他批次、原分段或调用方输入。
	for i := range gotBatches {
		gotBatches[i].texts[0], gotBatches[i].parts[0] = "synthetic-direct-mutation", -1
		for j := range gotBatches {
			if j != i && !reflect.DeepEqual(gotBatches[j], wantBatches[j]) {
				t.Fatal("直接修改批次元素覆盖了其他批次")
			}
		}
		if !reflect.DeepEqual(gotParts, wantParts) || !slices.Equal(texts, before) {
			t.Fatal("直接修改批次元素影响了原分段或输入文本")
		}
		gotBatches[i].texts[0], gotBatches[i].parts[0] = wantBatches[i].texts[0], wantBatches[i].parts[0]
		extendedTexts := append(gotBatches[i].texts, "synthetic-append")
		extendedIndexes := append(gotBatches[i].parts, -1)
		extendedTexts[0], extendedIndexes[0] = "synthetic-mutation", -1
		for j := range gotBatches {
			if j != i && !reflect.DeepEqual(gotBatches[j], wantBatches[j]) {
				t.Fatal("修改或追加一个批次覆盖了其他批次")
			}
		}
		gotBatches[i].texts = slices.Clone(wantBatches[i].texts)
		gotBatches[i].parts = slices.Clone(wantBatches[i].parts)
	}
	if !reflect.DeepEqual(gotParts, wantParts) || !slices.Equal(texts, before) {
		t.Fatal("批次缓冲修改影响了原分段或输入文本")
	}
}

func TestTencentBatchBufferCompatibility(t *testing.T) {
	for _, tc := range tencentBatchBufferInputs() {
		t.Run(tc.name, func(t *testing.T) {
			checkTencentBatchBufferCompatibility(t, tc.texts)
		})
	}
	pool := []string{"", "word", " 水杉😀 ", strings.Repeat("a", 5999), strings.Repeat("b", 6000), strings.Repeat(" ", 8192), strings.Repeat("中", 2730), strings.Repeat("😀", 2048), strings.Repeat("word ", 1600)}
	rng := rand.New(rand.NewSource(1146))
	for range 1000 {
		texts := make([]string, rng.Intn(translationBatchLimit+1))
		for i := range texts {
			texts[i] = pool[rng.Intn(len(pool))]
		}
		checkTencentBatchBufferCompatibility(t, texts)
	}
}

func TestTencentBatchBufferAllocationBudget(t *testing.T) {
	for _, count := range []int{9, translationBatchLimit} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			texts := make([]string, count)
			for i := range texts {
				texts[i] = "synthetic-word-" + strconv.Itoa(i)
			}
			var parts []translationPart
			var batches []tencentTranslationBatch
			allocations := testing.AllocsPerRun(100, func() {
				parts, batches = tencentTranslationBatches(texts)
			})
			if len(parts) != count || len(batches) != 1 || !slices.Equal(batches[0].texts, texts) {
				t.Fatal("短词批次输出变化")
			}
			if allocations > 4 {
				t.Fatalf("短词批次分配 %.0f 次，超过分段、文本、下标和批次各一次的预算", allocations)
			}
		})
	}
}

func BenchmarkTencentBatchBuffer(b *testing.B) {
	for _, tc := range tencentBatchBufferInputs() {
		for _, impl := range []struct {
			name  string
			build func([]string) ([]translationPart, []tencentTranslationBatch)
		}{{"previous", previousTencentTranslationBatches}, {"current", tencentTranslationBatches}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var parts []translationPart
				var batches []tencentTranslationBatch
				for b.Loop() {
					parts, batches = impl.build(tc.texts)
				}
				wantParts, wantBatches := previousTencentTranslationBatches(tc.texts)
				if !reflect.DeepEqual(parts, wantParts) || !reflect.DeepEqual(batches, wantBatches) {
					b.Fatal("批次输出变化")
				}
			})
		}
	}
}
