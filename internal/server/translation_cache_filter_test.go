package server

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestUncacheableTranslationsAllocationBudget(t *testing.T) {
	s := Server{accounts: &account.Service{}}
	ctx := context.Background()
	for _, size := range []int{1, 9, translationBatchLimit} {
		for _, text := range []string{"", strings.Repeat("长", translationCacheTextBytes), "两\n行", "\xff"} {
			t.Run(fmt.Sprintf("lookup/%d/%q", size, text), func(t *testing.T) {
				texts := slices.Repeat([]string{text}, size)
				var got map[string]string
				allocations := testing.AllocsPerRun(50, func() { got = s.cachedTranslations(ctx, "zh", "en", texts) })
				if got != nil || allocations != 0 {
					t.Fatalf("整批不可缓存时应返回 nil 且零分配：结果=%v，分配=%.0f", got, allocations)
				}
			})
		}
		for _, pair := range [][2]string{{"", "word"}, {strings.Repeat("长", translationCacheTextBytes), "long"}, {"两\n行", "lines"}, {"\xff", "invalid"}, {"词", ""}, {"词", strings.Repeat("a", translationCacheGlossBytes+1)}, {"词", "\xff"}, {"word", "word"}} {
			t.Run(fmt.Sprintf("store/%d/%q/%q", size, pair[0], pair[1]), func(t *testing.T) {
				sources, targets := slices.Repeat([]string{pair[0]}, size), slices.Repeat([]string{pair[1]}, size)
				allocations := testing.AllocsPerRun(50, func() { s.storeTranslations(ctx, "zh", "en", sources, targets) })
				if allocations != 0 {
					t.Fatalf("整批不可缓存时回写仍分配 %.0f 次", allocations)
				}
			})
		}
	}
}

func TestTranslationCacheStoreRejectsShortTargets(t *testing.T) {
	s := Server{accounts: &account.Service{}}
	for _, sources := range [][]string{{""}, {"词", ""}, {"", "词"}} {
		t.Run(fmt.Sprint(sources), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("译文不足时应触发索引越界")
				}
			}()
			s.storeTranslations(context.Background(), "zh", "en", sources, make([]string, len(sources)-1))
		})
	}
}

func TestTranslationCacheFiltersMixedBatches(t *testing.T) {
	disposableSchema(t)
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	service, err := account.New(t.Context(), account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	s := Server{accounts: service}
	long := strings.Repeat("a", translationCacheTextBytes+1)
	for n, tc := range []struct {
		sources, targets []string
		want             map[string]string
	}{
		{[]string{"一", "二"}, []string{"one", "two"}, map[string]string{"一": "one", "二": "two"}},
		{[]string{long, "", "一", "same", "二", "三", "四", "五", "\xff", "两\n行"}, []string{"long", "empty", "one", "same", "two", "", strings.Repeat("x", translationCacheGlossBytes+1), "\xff", "invalid", "lines"}, map[string]string{"一": "one", "二": "two"}},
		{[]string{"", long, "末"}, []string{"empty", "long", "last"}, map[string]string{"末": "last"}},
		{[]string{long, "same"}, []string{"long", "same"}, map[string]string{}},
		{nil, nil, map[string]string{}},
	} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			lang := fmt.Sprintf("test-%d", n)
			sourcesBefore, targetsBefore := slices.Clone(tc.sources), slices.Clone(tc.targets)
			s.storeTranslations(t.Context(), lang, "en", tc.sources, tc.targets)
			// 全量读取使用独立的有效 UTF-8 查询列表，避免将不可缓存的非法字节交给 PostgreSQL。
			query := []string{"一", "二", "三", "四", "五", "末", "same", "", long, "两\n行"}
			got, err := service.TranslationCacheLookup(t.Context(), lang, "en", query)
			if err != nil || !maps.Equal(got, tc.want) {
				t.Fatalf("过滤或译文配对错误：结果=%v，期望=%v，错误=%v", got, tc.want, err)
			}
			if !slices.Equal(tc.sources, sourcesBefore) || !slices.Equal(tc.targets, targetsBefore) {
				t.Fatal("回写修改了原始输入")
			}
			// 直接写入本应过滤的原文，确认读路径不会将它们带回。
			if err := service.TranslationCacheStore(t.Context(), lang, "en", []string{long, "", "两\n行"}, []string{"long", "empty", "lines"}); err != nil {
				t.Fatal(err)
			}
			lookup := append([]string{long, "", "两\n行", "\xff"}, tc.sources...)
			lookupBefore := slices.Clone(lookup)
			got = s.cachedTranslations(t.Context(), lang, "en", lookup)
			if !maps.Equal(got, tc.want) {
				t.Fatalf("查询未过滤不可缓存原文：%v，期望=%v", got, tc.want)
			}
			if !slices.Equal(lookup, lookupBefore) {
				t.Fatal("查询修改了原始输入")
			}
		})
	}
}

func BenchmarkTranslationCacheFiltering(b *testing.B) {
	s := Server{accounts: &account.Service{}}
	ctx := context.Background()
	for _, size := range []int{1, 9, translationBatchLimit} {
		for _, mode := range []string{"uncacheable", "identical", "cacheable"} {
			sources, targets := make([]string, size), make([]string, size)
			for i := range sources {
				sources[i], targets[i] = "词", "word"
				if mode == "uncacheable" {
					sources[i] = strings.Repeat("a", translationCacheTextBytes+1)
				}
				if mode == "identical" {
					targets[i] = sources[i]
				}
			}
			for _, impl := range []struct {
				name   string
				lookup func(*Server, context.Context, string, string, []string) map[string]string
				store  func(*Server, context.Context, string, string, []string, []string)
			}{{"previous", previousCachedTranslations, previousStoreTranslations}, {"current", (*Server).cachedTranslations, (*Server).storeTranslations}} {
				b.Run(fmt.Sprintf("%d/%s/%s", size, mode, impl.name), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						impl.lookup(&s, ctx, "zh", "en", sources)
						impl.store(&s, ctx, "zh", "en", sources, targets)
					}
				})
			}
		}
	}
}
