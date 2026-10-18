package server

import (
	"net/url"
	"strings"
	"testing"
)

func previousDeepLTranslationPrefix(source, target string) string {
	form := url.Values{"target_lang": {strings.ToUpper(target)}}
	if !strings.EqualFold(source, "auto") {
		form.Set("source_lang", strings.ToUpper(source))
	}
	return form.Encode()
}

func TestDeepLLanguageFieldAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		source, target string
		limit          float64
	}{
		{"zh", "en", 5},
		{"ZH", "EN", 3},
		{"auto", "en", 4},
	} {
		t.Run(tc.source+"/"+tc.target, func(t *testing.T) {
			v := translationRequest{Texts: []string{"word"}, Source: tc.source, Target: tc.target}
			var batches []deepLTranslationBatch
			allocations := testing.AllocsPerRun(50, func() { batches = deepLTranslationBatches(v) })
			if len(batches) != 1 || batches[0].body != previousDeepLTranslationPrefix(tc.source, tc.target)+"&text=word" {
				t.Fatal("语言字段或正文编码改变")
			}
			if allocations > tc.limit {
				t.Fatalf("语言参数 %s/%s 的单条请求仍分配 %.0f 次，期望至多 %.0f 次", tc.source, tc.target, allocations, tc.limit)
			}
		})
	}
}

func TestDeepLTranslationPrefixMatchesFormEncoding(t *testing.T) {
	languages := []string{"", "zh", "ZH", "en-US", "auto", "AuTo", "ß", "语言 😀 +%&=\n\x00", "\xff\xfe"}
	for _, source := range languages {
		for _, target := range languages {
			want := previousDeepLTranslationPrefix(source, target)
			if got := deepLTranslationPrefix(source, target); got != want {
				t.Fatalf("语言字段编码改变：来源=%q，目标=%q，得到=%q，期望=%q", source, target, got, want)
			}
		}
	}
}

func BenchmarkDeepLTranslationPrefix(b *testing.B) {
	for _, tc := range []struct{ source, target string }{{"zh", "en"}, {"ZH", "EN"}, {"auto", "en"}} {
		for _, impl := range []struct {
			name   string
			prefix func(string, string) string
		}{{"previous", previousDeepLTranslationPrefix}, {"current", deepLTranslationPrefix}} {
			b.Run(tc.source+"/"+tc.target+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var prefix string
				for b.Loop() {
					prefix = impl.prefix(tc.source, tc.target)
				}
				if prefix != previousDeepLTranslationPrefix(tc.source, tc.target) {
					b.Fatal("语言字段改变")
				}
			})
		}
	}
}
