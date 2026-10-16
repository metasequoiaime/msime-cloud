package account

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSensitiveCachePriorityOrder(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_sensitive_words RESTART IDENTITY CASCADE;
 INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES
 ('合成复核一','custom','review','test@example.test'),
 ('合成拦截一','custom','block','test@example.test'),
 ('合成复核二','custom','review','test@example.test'),
 ('合成拦截二','custom','block','test@example.test')`); err != nil {
		t.Fatal(err)
	}
	a := &Service{store: db}
	check := func(want []int64) {
		t.Helper()
		words, _, err := a.sensitive.list(t.Context(), db, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, word := range words {
			ids = append(ids, word.hit.WordID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("缓存规则顺序=%v，期望=%v", ids, want)
		}
		for _, matcher := range []SensitiveMatcher{a.SensitivePreview(), a.Sensitive()} {
			hits, err := matcher.Match(t.Context(), "合成复核一 合成拦截一 合成复核二 合成拦截二")
			if err != nil {
				t.Fatal(err)
			}
			ids = ids[:0]
			for _, hit := range hits {
				ids = append(ids, hit.WordID)
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("命中顺序=%v，期望=%v", ids, want)
			}
		}
	}
	check([]int64{2, 4, 1, 3})
	if _, err := db.pool.Exec(t.Context(), `UPDATE admin_sensitive_words SET level='block' WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	a.sensitive.invalidate()
	check([]int64{2, 3, 4, 1})
}

func BenchmarkSensitiveMatcherCachedPriority(b *testing.B) {
	text := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 3)
	for _, size := range []int{1, 16, 128, 1024} {
		words := make([]sensitiveWord, 0, size)
		for length := 1; len(words) < size; length++ {
			for offset := 0; offset < 26 && len(words) < size; offset++ {
				pattern := text[offset : offset+length]
				level := SensitiveReview
				if len(words)%2 == 1 {
					level = SensitiveBlock
				}
				words = append(words, sensitiveWord{hit: SensitiveHit{WordID: int64(len(words) + 1), Pattern: pattern, Level: level}, plain: pattern})
			}
		}
		for _, kind := range []string{"all", "none", "single"} {
			input := text
			if kind == "none" {
				input = "合成不命中文本"
			}
			if kind == "single" {
				input = "a"
			}
			for _, previous := range []bool{true, false} {
				implementation := "Current"
				if previous {
					implementation = "Previous"
				}
				b.Run(strconv.Itoa(size)+"/"+kind+"/"+implementation, func(b *testing.B) {
					a := cachedSensitiveService(words)
					if previous {
						// 旧流程按 ID 扫描规则，再对每次请求的命中排序；加载时间不计入两者的匹配基准。
						a.sensitive.words = words
						a.sensitive.plain = nil
					}
					matcher := a.SensitivePreview()
					b.ReportAllocs()
					for b.Loop() {
						hits, err := matcher.Match(b.Context(), input)
						if previous && err == nil && len(hits) > 0 {
							slices.SortStableFunc(hits, func(x, y SensitiveHit) int {
								return sensitiveLevelRank(x.Level) - sensitiveLevelRank(y.Level)
							})
						}
						if err != nil {
							b.Fatal(err)
						}
						want := size
						if kind == "none" {
							want = 0
						}
						if kind == "single" {
							want = 1
						}
						if len(hits) != want {
							b.Fatalf("命中数=%d，期望=%d", len(hits), want)
						}
					}
				})
			}
		}
	}
}
