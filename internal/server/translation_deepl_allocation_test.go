package server

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestDeepLTranslationBatchEscapeAllocationBudget(t *testing.T) {
	v := translationRequest{Texts: make([]string, translationBatchLimit), Source: "zh", Target: "en"}
	for i := range v.Texts {
		v.Texts[i] = strings.Repeat("中", 650)
	}
	allocations := testing.AllocsPerRun(20, func() {
		batches := deepLTranslationBatches(v)
		if len(batches) == 0 {
			t.Fatal("未生成请求")
		}
	})
	if allocations > 45 {
		t.Fatalf("长正文批量构造分配 %.0f 次，超过 45 次预算", allocations)
	}
}

func previousDeepLTranslationBatches(v translationRequest) []deepLTranslationBatch {
	form := url.Values{"target_lang": {strings.ToUpper(v.Target)}}
	if !strings.EqualFold(v.Source, "auto") {
		form.Set("source_lang", strings.ToUpper(v.Source))
	}
	prefix := form.Encode()
	body, start := prefix, 0
	texts := v.list()
	var batches []deepLTranslationBatch
	for i, text := range texts {
		field := "&text=" + url.QueryEscape(text)
		if len(body)+len(field) > deepLRequestBodyBytes && i > start {
			batches = append(batches, deepLTranslationBatch{body: body, texts: texts[start:i]})
			body, start = prefix, i
		}
		body += field
	}
	return append(batches, deepLTranslationBatch{body: body, texts: texts[start:]})
}

func TestDeepLTranslationBatchAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, text  string
		size, limit int
	}{
		{"单条短词", "word", 1, 8},
		{"九条短词", "word", 9, 12},
		{"整页短词", "word", translationBatchLimit, 12},
		{"九条正文", strings.Repeat("中", 2000), 9, 33},
		{"整页正文", strings.Repeat("中", 650), translationBatchLimit, 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := translationRequest{Texts: make([]string, tc.size), Source: "zh", Target: "en"}
			for i := range v.Texts {
				v.Texts[i] = tc.text
			}
			var batches []deepLTranslationBatch
			allocations := testing.AllocsPerRun(20, func() { batches = deepLTranslationBatches(v) })
			count := 0
			for _, batch := range batches {
				count += len(batch.texts)
			}
			if count != tc.size {
				t.Fatal("批量构建丢失文本")
			}
			if allocations > float64(tc.limit) {
				t.Fatalf("构建 %d 条批量仍分配 %.0f 次，期望至多 %d 次", tc.size, allocations, tc.limit)
			}
		})
	}
}

func TestDeepLTranslationBatchBuilderMatchesPrevious(t *testing.T) {
	for _, v := range []translationRequest{
		{Text: "单条 😀 +%", Source: "zh", Target: "en"},
		{Text: "忽略", Texts: []string{"第一条", "second +%", "😀"}, Source: "auto", Target: "en"},
		{Source: "auto", Target: "en"},
	} {
		if got, want := deepLTranslationBatches(v), previousDeepLTranslationBatches(v); !reflect.DeepEqual(got, want) {
			t.Fatal("单条、混合文本或空文本的构建结果改变")
		}
	}
	for _, source := range []string{"zh", "AUTO", "en-US"} {
		for _, size := range []int{1, 9, translationBatchLimit} {
			for _, text := range []string{"", "word", " ", "word word", "词 😀 +%&=\n\x00", strings.Repeat("%", 8192), strings.Repeat("中", 2730)} {
				v := translationRequest{Texts: make([]string, size), Source: source, Target: "zh-Hans"}
				for i := range v.Texts {
					v.Texts[i] = text
				}
				want := previousDeepLTranslationBatches(v)
				got := deepLTranslationBatches(v)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("构建结果不一致：来源=%s，条数=%d，单条字节=%d", source, size, len(text))
				}
			}
		}
	}
}

func BenchmarkDeepLTranslationBatchBuilder(b *testing.B) {
	for _, tc := range []struct{ name, text string }{
		{"短词", "word"},
		{"中文", "词语"},
		{"长正文", strings.Repeat("中", 650)},
	} {
		for _, size := range []int{1, 9, translationBatchLimit} {
			v := translationRequest{Texts: make([]string, size), Source: "zh", Target: "en"}
			for i := range v.Texts {
				v.Texts[i] = tc.text
			}
			for _, impl := range []struct {
				name  string
				build func(translationRequest) []deepLTranslationBatch
			}{{"previous", previousDeepLTranslationBatches}, {"current", deepLTranslationBatches}} {
				b.Run(fmt.Sprintf("%s/%d/%s", tc.name, size, impl.name), func(b *testing.B) {
					b.ReportAllocs()
					var batches []deepLTranslationBatch
					for b.Loop() {
						batches = impl.build(v)
					}
					if len(batches) == 0 {
						b.Fatal("未生成请求")
					}
				})
			}
		}
	}
}
