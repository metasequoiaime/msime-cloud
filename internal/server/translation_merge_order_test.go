package server

import (
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// previousMergeTranslations 保留本轮优化前的映射合并实现，核对顺序、缓存优先级和分配。
func previousMergeTranslations(wanted []string, cached map[string]string, missing, fresh []string) []string {
	translated := make(map[string]string, len(missing))
	for i, text := range missing {
		translated[text] = fresh[i]
	}
	out := make([]string, len(wanted))
	for i, text := range wanted {
		if value, hit := cached[text]; hit {
			out[i] = value
			continue
		}
		out[i] = translated[text]
	}
	return out
}

func mergeTranslationInputs(count int, mode string) ([]string, map[string]string, []string, []string) {
	wanted := make([]string, count)
	var cached map[string]string
	if mode == "mixed" || mode == "cached" {
		cached = make(map[string]string)
	}
	for i := range wanted {
		index := i
		if mode == "duplicate" {
			index %= max(1, count/2)
		}
		wanted[i] = "synthetic-word-" + strconv.Itoa(index)
		if mode == "cached" || mode == "mixed" && i%2 == 0 {
			cached[wanted[i]] = "cached-" + strconv.Itoa(i)
		}
	}
	missing := missingTexts(wanted, cached)
	fresh := make([]string, len(missing))
	for i, text := range missing {
		fresh[i] = "fresh-" + text
	}
	return wanted, cached, missing, fresh
}

func checkMergeTranslationCompatibility(t *testing.T, wanted []string, cached map[string]string, missing, fresh []string) {
	t.Helper()
	beforeWanted, beforeMissing, beforeFresh := slices.Clone(wanted), slices.Clone(missing), slices.Clone(fresh)
	beforeCached := maps.Clone(cached)
	got := mergeTranslations(wanted, cached, missing, fresh)
	want := previousMergeTranslations(wanted, cached, missing, fresh)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("译文顺序或缓存优先级变化：结果=%v，预期=%v", got, want)
	}
	if len(got) > 0 {
		// 返回切片必须独立持有元素，不能把调用方的译文缓冲直接交出去。
		got[0] = "synthetic-output-mutation"
	}
	if !slices.Equal(wanted, beforeWanted) || !slices.Equal(missing, beforeMissing) || !slices.Equal(fresh, beforeFresh) || !maps.Equal(cached, beforeCached) {
		t.Fatal("合并结果与输入共享了可变切片，或修改了输入")
	}
}

func TestMergeTranslationsOrderedCompatibility(t *testing.T) {
	for _, count := range []int{0, 1, 9, translationBatchLimit} {
		for _, mode := range []string{"uncached", "duplicate", "mixed", "cached"} {
			t.Run(strconv.Itoa(count)+"/"+mode, func(t *testing.T) {
				wanted, cached, missing, fresh := mergeTranslationInputs(count, mode)
				checkMergeTranslationCompatibility(t, wanted, cached, missing, fresh)
			})
		}
	}
	checkMergeTranslationCompatibility(t, nil, nil, nil, nil)
	checkMergeTranslationCompatibility(t, []string{"水杉", ""}, map[string]string{}, []string{"水杉", ""}, []string{"cedar", "empty"})
	checkMergeTranslationCompatibility(t, []string{"a", "b"}, nil, []string{"b", "a"}, []string{"B", "A"})
	checkMergeTranslationCompatibility(t, []string{"a", "b"}, map[string]string{"a": ""}, []string{"a", "b"}, []string{"A", "B"})
	rng := rand.New(rand.NewSource(1145))
	for range 1000 {
		wanted := make([]string, rng.Intn(translationBatchLimit+1))
		cached := make(map[string]string)
		for i := range wanted {
			wanted[i] = strconv.Itoa(rng.Intn(translationBatchLimit))
			if rng.Intn(3) == 0 {
				cached[wanted[i]] = "cached-" + wanted[i]
			}
		}
		missing := missingTexts(wanted, cached)
		if rng.Intn(2) == 0 {
			rng.Shuffle(len(missing), func(i, j int) { missing[i], missing[j] = missing[j], missing[i] })
		}
		fresh := make([]string, len(missing))
		for i, text := range missing {
			fresh[i] = "fresh-" + text
		}
		checkMergeTranslationCompatibility(t, wanted, cached, missing, fresh)
	}
}

func TestMergeTranslationsOrderedAllocationBudget(t *testing.T) {
	for _, count := range []int{9, translationBatchLimit} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			wanted, cached, missing, fresh := mergeTranslationInputs(count, "uncached")
			var got []string
			allocations := testing.AllocsPerRun(100, func() {
				got = mergeTranslations(wanted, cached, missing, fresh)
			})
			if !slices.Equal(got, fresh) {
				t.Fatal("整批未命中的译文结果变化")
			}
			if allocations > 1 {
				t.Fatalf("整批未命中合并分配 %.0f 次，超过独立输出切片的一次预算", allocations)
			}
		})
	}
}

func TestMergeTranslationsRejectsShortFresh(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(strconv.Itoa(capacity), func(t *testing.T) {
			fresh := make([]string, 1, capacity)
			defer func() {
				if recover() == nil {
					t.Fatal("译文长度不足时必须拒绝，不能读取容量范围内的隐藏元素")
				}
			}()
			mergeTranslations([]string{"a", "b"}, nil, []string{"a", "b"}, fresh)
		})
	}
}

func BenchmarkMergeTranslationsOrdered(b *testing.B) {
	for _, tc := range []struct {
		count int
		mode  string
	}{{1, "uncached"}, {9, "uncached"}, {translationBatchLimit, "uncached"}, {translationBatchLimit, "duplicate"}, {translationBatchLimit, "mixed"}, {translationBatchLimit, "cached"}} {
		wanted, cached, missing, fresh := mergeTranslationInputs(tc.count, tc.mode)
		for _, impl := range []struct {
			name  string
			merge func([]string, map[string]string, []string, []string) []string
		}{{"previous", previousMergeTranslations}, {"current", mergeTranslations}} {
			b.Run(strconv.Itoa(tc.count)+"/"+tc.mode+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got []string
				for b.Loop() {
					got = impl.merge(wanted, cached, missing, fresh)
				}
				if !reflect.DeepEqual(got, previousMergeTranslations(wanted, cached, missing, fresh)) {
					b.Fatal("合并结果变化")
				}
			})
		}
	}
}
