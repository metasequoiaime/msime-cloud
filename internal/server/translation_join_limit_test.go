package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func previousJoinTranslationParts(parts []translationPart, count int) ([]string, bool) {
	texts := make([]string, count)
	for _, part := range parts {
		texts[part.index] += part.prefix + part.translated + part.suffix
	}
	for _, text := range texts {
		if !bounded(text, contract.OutputTextBytes) {
			return nil, false
		}
	}
	return texts, true
}

func TestJoinTranslationPartsLimitCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []translationPart
		count int
	}{
		{"empty", nil, 0},
		{"missing", nil, 1},
		{"empty_parts", []translationPart{{}, {translated: "word"}, {}}, 1},
		{"blank", []translationPart{{prefix: "\t", translated: "\u2003", suffix: "\n"}}, 1},
		{"raw_bytes", []translationPart{{prefix: " ", translated: "\xff😀", suffix: "\n"}}, 1},
		{"interleaved", []translationPart{{index: 1, translated: "乙"}, {index: 0, translated: "甲"}, {index: 1, translated: "丁"}, {index: 0, translated: "丙"}}, 2},
		{"missing_index", []translationPart{{index: 0, translated: "词"}, {index: 2, translated: "word"}}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := joinTranslationParts(tc.parts, tc.count)
			want, wantOK := previousJoinTranslationParts(tc.parts, tc.count)
			if ok != wantOK || !reflect.DeepEqual(got, want) {
				t.Fatalf("拼接结果变化：结果=%q/%v，期望=%q/%v", got, ok, want, wantOK)
			}
		})
	}
	for _, padding := range []string{"", " \n", "\u2003😀"} {
		for _, delta := range []int{-1, 0, 1} {
			size := contract.OutputTextBytes - len(padding) + delta
			parts := []translationPart{{prefix: padding, translated: strings.Repeat("a", size/2)}, {translated: strings.Repeat("b", size-size/2)}}
			got, ok := joinTranslationParts(parts, 1)
			want, wantOK := previousJoinTranslationParts(parts, 1)
			if ok != wantOK || !reflect.DeepEqual(got, want) || ok != (delta <= 0) {
				t.Fatalf("输出上限边界变化：前缀=%q，偏移=%d，状态=%v/%v", padding, delta, ok, wantOK)
			}
		}
	}
}

func TestJoinTranslationPartsRejectsBeforeCopy(t *testing.T) {
	text := strings.Repeat("a", 40000)
	parts := []translationPart{{translated: text}, {translated: text}}
	var got []string
	var ok bool
	allocations := testing.AllocsPerRun(50, func() { got, ok = joinTranslationParts(parts, 1) })
	if ok || got != nil {
		t.Fatal("超长拼接结果未拒绝")
	}
	if allocations > 1 {
		t.Fatalf("拒绝超长结果仍分配 %.0f 次，期望不分配拼接正文", allocations)
	}
}

func BenchmarkJoinTranslationPartsLimit(b *testing.B) {
	cases := []struct {
		name  string
		parts []translationPart
		count int
	}{
		{"two_parts", []translationPart{{translated: strings.Repeat("a", 4096)}, {translated: strings.Repeat("b", 4096)}}, 1},
		{"exact_limit", []translationPart{{translated: strings.Repeat("a", contract.OutputTextBytes/2)}, {translated: strings.Repeat("b", contract.OutputTextBytes/2)}}, 1},
		{"oversize", []translationPart{{translated: strings.Repeat("a", 40000)}, {translated: strings.Repeat("b", 40000)}}, 1},
		{"oversize_prefix", []translationPart{{prefix: "\n", translated: strings.Repeat("a", contract.OutputTextBytes)}}, 1},
	}
	for _, size := range []int{1, 9, translationBatchLimit} {
		parts := make([]translationPart, size)
		for i := range parts {
			parts[i] = translationPart{index: i, translated: "water fir"}
		}
		cases = append(cases, struct {
			name  string
			parts []translationPart
			count int
		}{fmt.Sprintf("words_%d", size), parts, size})
	}
	for _, tc := range cases {
		want, wantOK := previousJoinTranslationParts(tc.parts, tc.count)
		for _, impl := range []struct {
			name string
			join func([]translationPart, int) ([]string, bool)
		}{{"previous", previousJoinTranslationParts}, {"current", joinTranslationParts}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got []string
				var ok bool
				for b.Loop() {
					got, ok = impl.join(tc.parts, tc.count)
				}
				if ok != wantOK || !reflect.DeepEqual(got, want) {
					b.Fatal("拼接结果变化")
				}
			})
		}
	}
}
