package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func cachedSensitiveService(words []sensitiveWord) *Service {
	a := &Service{}
	// 测试和基准只测已缓存规则，避免长基准进入数据库刷新路径。
	a.sensitive.words, a.sensitive.loaded = words, time.Now().AddDate(100, 0, 0)
	return a
}

func TestSensitiveMatcherNormalizesOnlyNeededForms(t *testing.T) {
	re, err := compileSensitiveRegex("123456789")
	if err != nil {
		t.Fatal(err)
	}
	plain := sensitiveWord{plain: "never-match"}
	regex := sensitiveWord{re: re}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name  string
		words []sensitiveWord
		text  string
		ctx   context.Context
		alloc float64
	}{
		{"plain", []sensitiveWord{plain, plain}, strings.Repeat("正常内容 ＡＢＣ\u200b ", 100), t.Context(), 1},
		{"empty-fold", []sensitiveWord{plain, plain}, strings.Repeat(" \u200b　", 100), t.Context(), 1},
		{"cancelled", []sensitiveWord{plain, regex}, strings.Repeat("正常内容 ＡＢＣ\u200b ", 100), ctx, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matcher := cachedSensitiveService(tc.words).SensitivePreview()
			var matchErr error
			var hits []SensitiveHit
			allocs := testing.AllocsPerRun(20, func() { hits, matchErr = matcher.Match(tc.ctx, tc.text) })
			if hits != nil || !errors.Is(matchErr, tc.ctx.Err()) {
				t.Fatalf("无命中或取消语义错误：%v/%v", hits, matchErr)
			}
			if allocs > tc.alloc {
				t.Fatalf("仅需对应文本形式时分配了 %g 次，上限 %g 次", allocs, tc.alloc)
			}
		})
	}
}

func TestSensitiveMatcherNormalizationPreservesHits(t *testing.T) {
	words := []sensitiveWord{
		{hit: SensitiveHit{WordID: 1, Pattern: "加V", Level: SensitiveReview}, plain: "加v"},
		{hit: SensitiveHit{WordID: 2, Pattern: "好友", Level: SensitiveBlock}, plain: "好友"},
	}
	for i, pattern := range []string{`vx:\s*[a-z0-9_]{5,}`, `ＱＱ[0-9]{5,}`, `A B`} {
		re, err := compileSensitiveRegex(pattern)
		if err != nil {
			t.Fatal(err)
		}
		level := SensitiveReview
		if i == 0 {
			level = SensitiveBlock
		}
		words = append(words, sensitiveWord{hit: SensitiveHit{WordID: int64(i + 3), Pattern: pattern, Level: level}, re: re})
	}
	for _, tc := range []struct {
		name, text string
		ids        []int64
	}{
		{"plain", "加\u200bＶ 好友 加v", []int64{2, 1}},
		{"regex-folded", "ＶＸ: abc_12345", []int64{3}},
		{"regex-original-width", "ＱＱ123456", []int64{4}},
		{"regex-spaces", "A　B", []int64{5}},
		{"mixed", "加\u200bＶ 好友 VX: abc_12345 ＱＱ123456 A B 加v", []int64{2, 3, 1, 4, 5}},
		{"invalid-utf8", "\xff加v", []int64{1}},
		{"empty-fold", "　 \u200b", nil},
		{"no-hit", "普通内容", nil},
		{"empty", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, preview := range []bool{true, false} {
				a := cachedSensitiveService(words)
				matcher := sensitiveMatcher{a: a, preview: preview}
				for range 2 {
					hits, err := matcher.Match(t.Context(), tc.text)
					if err != nil {
						t.Fatal(err)
					}
					var ids []int64
					for _, hit := range hits {
						ids = append(ids, hit.WordID)
						if hit != words[hit.WordID-1].hit {
							t.Fatalf("匹配字段改变：%+v", hit)
						}
					}
					if !slices.Equal(ids, tc.ids) || (len(tc.ids) == 0 && hits != nil) {
						t.Fatalf("命中或稳定排序改变：%v，期望 %v", hits, tc.ids)
					}
				}
				counts := map[int64]int64{}
				for key, count := range a.sensitive.pending {
					counts[key.word] += count
				}
				if preview && len(counts) != 0 {
					t.Fatal("预览写入命中计数：", counts)
				}
				if !preview {
					if len(counts) != len(tc.ids) {
						t.Fatal("命中计数条数错误：", counts)
					}
					for _, id := range tc.ids {
						if counts[id] != 2 {
							t.Fatal("同一词每次请求只能计数一次：", counts)
						}
					}
				}
			}
		})
	}
}

func TestSensitiveRegexLiteralPrefixSafety(t *testing.T) {
	for _, tc := range []struct {
		pattern, want string
	}{
		{"never-match", "never-match"},
		{`never\.match`, "never.match"},
		{"ABC", ""},
		{`[a-z]+`, ""},
		{"ＱＱ", ""},
		{"好友", "好友"},
	} {
		if got := sensitiveRegexLiteral(tc.pattern); got != tc.want {
			t.Errorf("sensitiveRegexLiteral(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

func BenchmarkSensitiveMatcherNormalization(b *testing.B) {
	text := strings.Repeat("正常内容 ABC　\u200b", 10000)
	for _, kind := range []string{"plain", "regex", "mixed"} {
		b.Run(kind, func(b *testing.B) {
			var words []sensitiveWord
			for i := range 20 {
				pattern := fmt.Sprintf("never-match-%d", i)
				w := sensitiveWord{hit: SensitiveHit{Pattern: pattern}, plain: pattern}
				if kind == "regex" || (kind == "mixed" && i%2 == 0) {
					var err error
					w.re, err = compileSensitiveRegex(pattern)
					if err != nil {
						b.Fatal(err)
					}
					w.literal = sensitiveRegexLiteral(pattern)
				}
				words = append(words, w)
			}
			matcher := cachedSensitiveService(words).SensitivePreview()
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				if hits, err := matcher.Match(b.Context(), text); err != nil || hits != nil {
					b.Fatalf("匹配错误：%v/%v", hits, err)
				}
			}
		})
	}
}
