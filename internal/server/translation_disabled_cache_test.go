package server

import (
	"context"
	"fmt"
	"testing"
)

func previousCachedTranslations(s *Server, ctx context.Context, source, target string, texts []string) map[string]string {
	wanted := make([]string, 0, len(texts))
	for _, text := range texts {
		if cacheableText(text) {
			wanted = append(wanted, text)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, translationCacheTimeout)
	defer cancel()
	cached, err := s.accounts.TranslationCacheLookup(ctx, source, target, wanted)
	if err != nil {
		return nil
	}
	return cached
}

func previousStoreTranslations(s *Server, ctx context.Context, source, target string, sources, targets []string) {
	keep, values := make([]string, 0, len(sources)), make([]string, 0, len(sources))
	for i, text := range sources {
		if cacheablePair(text, targets[i]) {
			keep = append(keep, text)
			values = append(values, targets[i])
		}
	}
	if len(keep) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, translationCacheTimeout)
	defer cancel()
	_ = s.accounts.TranslationCacheStore(ctx, source, target, keep, values)
}

func TestDisabledTranslationCacheAllocationBudget(t *testing.T) {
	var s Server
	ctx := context.Background()
	for _, size := range []int{1, 9, translationBatchLimit} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			sources, targets := make([]string, size), make([]string, size)
			for i := range sources {
				sources[i] = fmt.Sprintf("词%d", i)
				targets[i] = fmt.Sprintf("gloss%d", i)
			}
			var got map[string]string
			allocations := testing.AllocsPerRun(50, func() {
				got = s.cachedTranslations(ctx, "zh", "en", sources)
				s.storeTranslations(ctx, "zh", "en", sources, targets)
			})
			if got != nil {
				t.Fatalf("缓存关闭时应返回 nil：%v", got)
			}
			if allocations != 0 {
				t.Fatalf("缓存关闭时读写仍分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func BenchmarkDisabledTranslationCache(b *testing.B) {
	var s Server
	ctx := context.Background()
	for _, size := range []int{1, 9, translationBatchLimit} {
		sources, targets := make([]string, size), make([]string, size)
		for i := range sources {
			sources[i] = fmt.Sprintf("词%d", i)
			targets[i] = fmt.Sprintf("gloss%d", i)
		}
		for _, impl := range []struct {
			name   string
			lookup func(*Server, context.Context, string, string, []string) map[string]string
			store  func(*Server, context.Context, string, string, []string, []string)
		}{
			{"previous", previousCachedTranslations, previousStoreTranslations},
			{"current", (*Server).cachedTranslations, (*Server).storeTranslations},
		} {
			b.Run(fmt.Sprintf("%d/%s", size, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				var got map[string]string
				for b.Loop() {
					got = impl.lookup(&s, ctx, "zh", "en", sources)
					impl.store(&s, ctx, "zh", "en", sources, targets)
				}
				if got != nil {
					b.Fatalf("缓存关闭时应返回 nil：%v", got)
				}
			})
		}
	}
}
