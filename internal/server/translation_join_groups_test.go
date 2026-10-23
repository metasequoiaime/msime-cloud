package server

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

func previousGroupedTranslationJoin(parts []translationPart, count int) ([]string, bool) {
	texts := make([]string, count)
	for _, part := range parts {
		if texts[part.index] == "" && part.prefix == "" && part.suffix == "" {
			texts[part.index] = part.translated
			continue
		}
		// 先检查累计字节数，避免为必然超限的结果分配并复制正文。
		if len(part.prefix)+len(part.translated)+len(part.suffix) > contract.OutputTextBytes-len(texts[part.index]) {
			return nil, false
		}
		texts[part.index] += part.prefix + part.translated + part.suffix
	}
	for _, text := range texts {
		if !bounded(text, contract.OutputTextBytes) {
			return nil, false
		}
	}
	return texts, true
}

type translationJoinGroupCase struct {
	name        string
	parts       []translationPart
	count       int
	allocations int
}

func translationJoinGroupInputs() []translationJoinGroupCase {
	var cases []translationJoinGroupCase
	for _, size := range []int{1, 9, translationBatchLimit} {
		words := make([]translationPart, size)
		for i := range words {
			words[i] = translationPart{index: i, translated: "word"}
		}
		cases = append(cases, translationJoinGroupCase{fmt.Sprintf("words_%d", size), words, size, 1})
		for _, segments := range []int{2, 3} {
			boundary := 4999
			if segments == 3 {
				boundary = 2499
			}
			source := strings.Repeat("a", boundary) + " " + strings.Repeat("b", 8192-boundary-1)
			sources := slices.Repeat([]string{source}, size)
			parts := translationParts(sources, niutransTextCharacters)
			if len(parts) != segments*size {
				panic("合成文本的实际分段数变化")
			}
			for i := range parts {
				parts[i].translated = strings.Repeat("word", 1024)
			}
			cases = append(cases, translationJoinGroupCase{fmt.Sprintf("parts_%d/items_%d", segments, size), parts, size, size + 1})
		}
	}
	cases = append(cases, translationJoinGroupCase{"two_plain", []translationPart{{translated: strings.Repeat("a", 4096)}, {translated: strings.Repeat("b", 4096)}}, 1, 2})
	return cases
}

func TestTranslationJoinGroupsAllocationBudget(t *testing.T) {
	for _, tc := range translationJoinGroupInputs() {
		t.Run(tc.name, func(t *testing.T) {
			want, wantOK := previousGroupedTranslationJoin(tc.parts, tc.count)
			before := slices.Clone(tc.parts)
			var got []string
			var ok bool
			allocations := testing.AllocsPerRun(50, func() { got, ok = joinTranslationParts(tc.parts, tc.count) })
			if ok != wantOK || !reflect.DeepEqual(got, want) {
				t.Fatal("分段拼接结果变化")
			}
			if !reflect.DeepEqual(tc.parts, before) {
				t.Fatal("拼接修改了原始分段")
			}
			if allocations > float64(tc.allocations) {
				t.Fatalf("分段拼接分配 %.0f 次，期望至多 %d 次", allocations, tc.allocations)
			}
		})
	}
}

func TestTranslationJoinGroupsCompatibility(t *testing.T) {
	random := rand.New(rand.NewPCG(23, 71))
	fragments := []string{"", "word", "\xff", "甲😀", " ", "\t\n", "\u2003"}
	for n := 0; n < 300; n++ {
		count := 1 + random.IntN(5)
		parts := make([]translationPart, random.IntN(count*4))
		for i := range parts {
			parts[i] = translationPart{index: random.IntN(count), prefix: fragments[random.IntN(len(fragments))], translated: fragments[random.IntN(len(fragments))], suffix: fragments[random.IntN(len(fragments))]}
		}
		got, ok := joinTranslationParts(parts, count)
		want, wantOK := previousGroupedTranslationJoin(parts, count)
		if ok != wantOK || !reflect.DeepEqual(got, want) {
			t.Fatalf("交错分段拼接变化：结果=%q/%v，期望=%q/%v", got, ok, want, wantOK)
		}
	}
	for _, parts := range [][]translationPart{
		{{translated: "a"}, {translated: "b"}, {index: 1, translated: "c"}, {translated: "d"}, {translated: "e"}},
		{{}, {translated: "word"}, {}},
		{{translated: strings.Repeat("a", contract.OutputTextBytes/2)}, {translated: strings.Repeat("b", contract.OutputTextBytes/2)}, {}},
		{{translated: strings.Repeat("a", contract.OutputTextBytes)}, {prefix: " "}, {}},
	} {
		for _, count := range []int{1, 2, translationBatchLimit + 1} {
			if count == 1 && slices.ContainsFunc(parts, func(p translationPart) bool { return p.index > 0 }) {
				continue
			}
			got, ok := joinTranslationParts(parts, count)
			want, wantOK := previousGroupedTranslationJoin(parts, count)
			if ok != wantOK || !reflect.DeepEqual(got, want) {
				t.Fatal("分组边界或输出上限变化")
			}
		}
	}
}

func TestTranslationJoinGroupsPreservesIndexPanic(t *testing.T) {
	for _, tc := range []struct {
		parts []translationPart
		count int
	}{
		{nil, -1},
		{[]translationPart{{translated: "word"}}, 0},
		{[]translationPart{{index: -1}, {index: -1}}, 1},
		{[]translationPart{{translated: "a"}, {translated: "b"}, {index: 1}}, 1},
	} {
		t.Run(fmt.Sprint(tc.count, tc.parts), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("非法分段下标或负结果数应触发 panic")
				}
			}()
			joinTranslationParts(tc.parts, tc.count)
		})
	}
}

func BenchmarkTranslationJoinGroups(b *testing.B) {
	for _, tc := range translationJoinGroupInputs() {
		want, wantOK := previousGroupedTranslationJoin(tc.parts, tc.count)
		for _, impl := range []struct {
			name string
			join func([]translationPart, int) ([]string, bool)
		}{{"previous", previousGroupedTranslationJoin}, {"current", joinTranslationParts}} {
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
