package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func previousTranslationTextChunks(text string, limit int) []string {
	var chunks []string
	for len(text) > 0 {
		end, boundary, units := len(text), 0, 0
		for i, r := range text {
			size := 1
			if r > 0xffff {
				size = 2
			}
			if units+size > limit {
				end = i
				break
			}
			units += size
			if units >= limit/2 && (unicode.IsSpace(r) || strings.ContainsRune("。！？.!?;；", r)) {
				boundary = i + utf8.RuneLen(r)
			}
		}
		if end < len(text) && boundary > 0 {
			end = boundary
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// previousTranslationParts 保留原条目下标及分段空白；空白片段留在本地，不调用上游。
func previousTranslationParts(texts []string, limit int) []translationPart {
	var parts []translationPart
	for index, text := range texts {
		chunks := previousTranslationTextChunks(text, limit)
		for _, chunk := range chunks {
			part := translationPart{index: index, text: chunk}
			if len(chunks) > 1 {
				part.text = strings.TrimLeftFunc(chunk, unicode.IsSpace)
				part.prefix = chunk[:len(chunk)-len(part.text)]
				trimmed := strings.TrimRightFunc(part.text, unicode.IsSpace)
				part.suffix, part.text = part.text[len(trimmed):], trimmed
			}
			parts = append(parts, part)
		}
	}
	return parts
}

func translationPartAllocationInputs() []struct {
	name  string
	texts []string
} {
	cases := []struct {
		name  string
		texts []string
	}{
		{"ascii_limit", []string{strings.Repeat("a", niutransTextCharacters)}},
		{"ascii_request", []string{strings.Repeat("a", 8192)}},
		{"chinese_request", []string{strings.Repeat("杉", 2730)}},
		{"emoji_request", []string{strings.Repeat("😀", 2048)}},
		{"paragraphs", []string{strings.Repeat("word ", 600) + "\n\n" + strings.Repeat("text ", 1000)}},
		{"mixed", []string{"词", strings.Repeat("a", 8192), "", " 词😀 ", strings.Repeat(" ", 6000) + "word"}},
	}
	for _, count := range []int{1, 9, translationBatchLimit} {
		texts := make([]string, count)
		for i := range texts {
			texts[i] = fmt.Sprintf("水杉%d", i)
		}
		cases = append(cases, struct {
			name  string
			texts []string
		}{fmt.Sprintf("words_%d", count), texts})
	}
	return cases
}

func TestTranslationPartsStreamCompatibility(t *testing.T) {
	cases := translationPartAllocationInputs()
	for _, texts := range [][]string{nil, {}, {"", ""}, {" \t词😀 \n", "", "second"}, {"\xffabc"}} {
		cases = append(cases, struct {
			name  string
			texts []string
		}{fmt.Sprintf("edge_%d", len(cases)), texts})
	}
	for _, limit := range []int{7, niutransTextCharacters, tencentBatchCharacters} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%d/%s", limit, tc.name), func(t *testing.T) {
				got, want := translationParts(tc.texts, limit), previousTranslationParts(tc.texts, limit)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("分段的内容、原下标或首尾空白变化：结果=%+v，期望=%+v", got, want)
				}
			})
		}
	}
	for _, limit := range []int{7, niutransTextCharacters, tencentBatchCharacters} {
		for offset := -4; offset <= 4; offset++ {
			for _, suffix := range []string{"a😀b", "。\n\tword", " \u2003😀杉\r\n", "！b", "？b", ".b", "!b", "?b", ";b", "；b"} {
				text := strings.Repeat("a", limit+offset) + suffix
				texts := []string{"前", text, "后"}
				if got, want := translationParts(texts, limit), previousTranslationParts(texts, limit); !reflect.DeepEqual(got, want) {
					t.Fatalf("UTF-16 边界分段变化：上限=%d，偏移=%d，后缀=%q", limit, offset, suffix)
				}
			}
		}
	}
}

func TestTranslationPartsStreamAllocationBudget(t *testing.T) {
	for _, size := range []int{1, 9, translationBatchLimit} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			texts := make([]string, size)
			for i := range texts {
				texts[i] = fmt.Sprintf("水杉%d", i)
			}
			var got []translationPart
			allocations := testing.AllocsPerRun(50, func() { got = translationParts(texts, niutransTextCharacters) })
			if !reflect.DeepEqual(got, previousTranslationParts(texts, niutransTextCharacters)) {
				t.Fatal("分段内容变化")
			}
			if allocations > 1 {
				t.Fatalf("短文本批次分配 %.0f 次，期望只分配一次结果切片", allocations)
			}
		})
	}
}

func TestTranslationChunkIteratorStops(t *testing.T) {
	for _, text := range []string{"词😀", strings.Repeat("a", 8192), strings.Repeat("word ", 1600)} {
		want := previousTranslationTextChunks(text, niutransTextCharacters)[0]
		var got string
		for chunk := range translationTextChunks(text, niutransTextCharacters) {
			got = chunk
			break
		}
		if got != want {
			t.Fatalf("首段变化：结果=%q，期望=%q", got, want)
		}
	}
}

func BenchmarkTranslationPartsStream(b *testing.B) {
	for _, tc := range translationPartAllocationInputs() {
		want := previousTranslationParts(tc.texts, niutransTextCharacters)
		for _, impl := range []struct {
			name  string
			split func([]string, int) []translationPart
		}{
			{"previous", previousTranslationParts}, {"current", translationParts},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got []translationPart
				for b.Loop() {
					got = impl.split(tc.texts, niutransTextCharacters)
				}
				if !reflect.DeepEqual(got, want) {
					b.Fatal("分段内容变化")
				}
			})
		}
	}
}
