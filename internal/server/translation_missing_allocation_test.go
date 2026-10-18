package server

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func previousMissingTexts(wanted []string, cached map[string]string) []string {
	seen := make(map[string]bool, len(wanted))
	missing := make([]string, 0, len(wanted))
	for _, text := range wanted {
		if _, hit := cached[text]; hit || seen[text] {
			continue
		}
		seen[text] = true
		missing = append(missing, text)
	}
	return missing
}

func TestMissingTextsCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wanted []string
		cached map[string]string
		want   []string
	}{
		{"nil", nil, nil, []string{}},
		{"empty", []string{}, map[string]string{}, []string{}},
		{"uncached", []string{"你", "好", "你", "😀"}, nil, []string{"你", "好", "😀"}},
		{"partial", []string{"你", "好", "你", "😀", "好"}, map[string]string{"你": "you"}, []string{"好", "😀"}},
		{"last_missing", []string{"你", "好", "你", "😀"}, map[string]string{"你": "you", "好": "good"}, []string{"😀"}},
		{"cached_suffix", []string{"😀", "你", "好", "😀"}, map[string]string{"你": "you", "好": "good"}, []string{"😀"}},
		{"empty_gloss", []string{"你", "好"}, map[string]string{"你": "", "好": "good"}, []string{}},
		{"empty_text", []string{"", "你", "", "\xff"}, nil, []string{"", "你", "\xff"}},
		{"cached_empty_text", []string{"", "你", ""}, map[string]string{"": ""}, []string{"你"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantedBefore, cachedBefore := slices.Clone(tc.wanted), maps.Clone(tc.cached)
			got := missingTexts(tc.wanted, tc.cached)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("缺失文本变化：结果=%q，期望非 nil 的 %q", got, tc.want)
			}
			if len(got) > 0 {
				got[0] = "修改输出"
			}
			if !slices.Equal(tc.wanted, wantedBefore) || !maps.Equal(tc.cached, cachedBefore) {
				t.Fatal("查找缺失文本修改了输入，或输出与输入共用底层数组")
			}
		})
	}
}

func TestMissingTextsCachedAllocationBudget(t *testing.T) {
	for _, size := range []int{1, 9, translationBatchLimit} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			wanted := make([]string, size)
			cached := make(map[string]string, size)
			for i := range wanted {
				wanted[i] = fmt.Sprintf("词%d", i)
				cached[wanted[i]] = "译文"
			}
			var got []string
			allocations := testing.AllocsPerRun(100, func() {
				got = missingTexts(wanted, cached)
			})
			if got == nil || len(got) != 0 {
				t.Fatalf("整批命中时应返回非 nil 空切片：%q", got)
			}
			if allocations != 0 {
				t.Fatalf("整批命中仍分配 %.0f 次，期望零分配", allocations)
			}
		})
	}
}

func BenchmarkMissingTextsAllocation(b *testing.B) {
	for _, size := range []int{1, 9, translationBatchLimit} {
		for _, mode := range []string{"cached", "uncached", "partial", "duplicates"} {
			wanted := make([]string, size)
			cached := make(map[string]string, size)
			for i := range wanted {
				key := i
				if mode == "duplicates" {
					key = i % 3
				}
				wanted[i] = fmt.Sprintf("词%d", key)
				if mode == "cached" || (mode == "partial" && i%2 == 0) {
					cached[wanted[i]] = "译文"
				}
			}
			want := previousMissingTexts(wanted, cached)
			for _, impl := range []struct {
				name string
				find func([]string, map[string]string) []string
			}{{"previous", previousMissingTexts}, {"current", missingTexts}} {
				b.Run(fmt.Sprintf("%d/%s/%s", size, mode, impl.name), func(b *testing.B) {
					b.ReportAllocs()
					var got []string
					for b.Loop() {
						got = impl.find(wanted, cached)
					}
					if got == nil || !slices.Equal(got, want) {
						b.Fatalf("缺失文本变化：结果=%q，期望=%q", got, want)
					}
				})
			}
		}
	}
}
