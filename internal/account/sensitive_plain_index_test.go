package account

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func sensitivePlainScanWords(count int, near bool) []sensitiveWord {
	words := make([]sensitiveWord, count)
	for i := range words {
		pattern := fmt.Sprintf("rule%04d", i)
		if near {
			pattern = strings.Repeat("a", 190) + "b" + fmt.Sprint(i)
		}
		level := SensitiveReview
		if i%2 == 0 {
			level = SensitiveBlock
		}
		words[i] = sensitiveWord{hit: SensitiveHit{WordID: int64(i + 1), Pattern: pattern, Level: level}, plain: pattern}
	}
	return words
}

func BenchmarkSensitivePlainScan(b *testing.B) {
	for _, count := range []int{16, 128, 1024} {
		for _, kind := range []string{"short", "ascii", "unicode", "near", "spread", "cjk", "boundary-ascii", "boundary-spread", "all"} {
			words := sensitivePlainScanWords(count, kind == "near")
			text := "rule0000"
			switch kind {
			case "ascii":
				text = strings.Repeat("safecontent", 32768)
			case "unicode":
				text = strings.Repeat("正常内容", 32768)
			case "spread", "boundary-spread":
				for i := range words {
					words[i].plain = fmt.Sprintf("%c%04d", 'a'+i%26, i)
					words[i].hit.Pattern = words[i].plain
				}
				text = strings.Repeat("safecontent", 32768)
			case "cjk":
				for i := range words {
					words[i].plain = fmt.Sprintf("%c敏感", rune(0x5000+(i*37)%4096))
					words[i].hit.Pattern = words[i].plain
				}
				text = strings.Repeat("正常内容", 32768)
			case "near":
				text = strings.Repeat("a", 350000)
			case "all":
				var patterns []string
				for _, word := range words {
					patterns = append(patterns, word.plain)
				}
				text = strings.Repeat("z", 4096) + strings.Join(patterns, " ")
			}
			if kind == "boundary-ascii" || kind == "boundary-spread" {
				text = strings.Repeat("safecontent", minSensitivePlainText/11+1)[:minSensitivePlainText]
			}
			for _, previous := range []bool{true, false} {
				implementation := "Current"
				if previous {
					implementation = "Previous"
				}
				b.Run(fmt.Sprintf("%d/%s/%s", count, kind, implementation), func(b *testing.B) {
					service := cachedSensitiveService(words)
					if previous {
						service.sensitive.plain = nil
					}
					matcher := service.SensitivePreview()
					b.ReportAllocs()
					b.SetBytes(int64(len(text)))
					for b.Loop() {
						hits, err := matcher.Match(b.Context(), text)
						if err != nil {
							b.Fatal(err)
						}
						want := 0
						if kind == "short" {
							want = 1
						}
						if kind == "all" {
							want = count
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

func sensitivePlainIndexFixture(patterns []string) []sensitiveWord {
	var words []sensitiveWord
	for _, pattern := range patterns {
		plain := foldSensitive(pattern, true)
		if plain != "" {
			words = append(words, sensitiveWord{plain: plain})
		}
	}
	for len(words) < minSensitivePlainRules {
		words = append(words, sensitiveWord{plain: fmt.Sprintf("never-index-rule-%d!", len(words))})
	}
	return words
}

func TestSensitivePlainIndexCompatibility(t *testing.T) {
	check := func(patterns []string, text string) {
		t.Helper()
		words := sensitivePlainIndexFixture(patterns)
		index := mustSensitivePlainIndex(t, words)
		if index == nil {
			t.Fatal("大量普通规则没有建立共享索引")
		}
		compact := foldSensitive(text, true)
		matches, err := index.match(t.Context(), compact)
		if err != nil {
			t.Fatal(err)
		}
		for i, word := range words {
			if got, want := matches.contains(i), strings.Contains(compact, word.plain); got != want {
				t.Fatalf("规则=%q，文本=%q，索引=%v，逐条匹配=%v", word.plain, text, got, want)
			}
		}
	}
	for _, tc := range []struct {
		patterns []string
		text     string
	}{
		{[]string{"he", "she", "his", "hers"}, "ushers"},
		{[]string{"a", "aa", "aaa", "ab", "bab", "bc", "bca", "c", "caa"}, "abccabaaaaa"},
		{[]string{"水杉", "杉", "😀水", "水杉😀", "水杉"}, "😀水杉😀水杉"},
		{[]string{"加V", "ｖ", "v", "好友", "好​友"}, "加 Ｖ 好​友"},
		{[]string{"�", "a�", "�b"}, "a\xffb"},
		{[]string{"never"}, ""},
	} {
		check(tc.patterns, tc.text)
	}
	random := rand.New(rand.NewSource(701))
	alphabet := []rune("abc水杉😀 Ａ​\ufffd")
	for range 1000 {
		makeText := func(size int) string {
			var b strings.Builder
			for range size {
				b.WriteRune(alphabet[random.Intn(len(alphabet))])
			}
			return b.String()
		}
		var patterns []string
		for range 32 + random.Intn(128) {
			patterns = append(patterns, makeText(1+random.Intn(12)))
		}
		check(patterns, makeText(random.Intn(512)))
	}
}

func TestSensitivePlainIndexChunkBoundary(t *testing.T) {
	words := sensitivePlainScanWords(minSensitivePlainRules, false)
	index := mustSensitivePlainIndex(t, words)
	for _, offset := range []int{4090, 4095, 4096, 8191, 8192} {
		text := strings.Repeat("z", offset) + "rule0000" + strings.Repeat("z", 8192) + "rule0001"
		matches, err := index.match(t.Context(), text)
		if err != nil {
			t.Fatal(err)
		}
		for i, word := range words {
			if matches.contains(i) != strings.Contains(text, word.plain) {
				t.Fatalf("分块边界 %d 的规则 %q 结果不符", offset, word.plain)
			}
		}
	}
}

func TestSensitivePlainIndexNoMatchAllocationBudget(t *testing.T) {
	index := mustSensitivePlainIndex(t, sensitivePlainScanWords(minSensitivePlainRules, false))
	if index == nil {
		t.Fatal("没有建立普通规则索引")
	}
	text := strings.Repeat("safecontent", 32768)
	var matches sensitivePlainMatches
	var err error
	allocations := testing.AllocsPerRun(20, func() { matches, err = index.match(t.Context(), text) })
	if err != nil || matches != nil {
		t.Fatalf("无命中结果=%v/%v", matches, err)
	}
	if allocations != 0 {
		t.Fatalf("无命中扫描分配 %.0f 次，期望零分配", allocations)
	}
}

type sensitivePlainCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
	limit  int
}

func (c *sensitivePlainCancelContext) Err() error {
	c.checks++
	if c.checks == c.limit {
		c.cancel()
	}
	return c.Context.Err()
}

func TestSensitivePlainIndexCancellation(t *testing.T) {
	index := mustSensitivePlainIndex(t, sensitivePlainScanWords(minSensitivePlainRules, false))
	if index == nil {
		t.Fatal("没有建立普通规则索引")
	}
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &sensitivePlainCancelContext{Context: parent, cancel: cancel, limit: 3}
	matches, err := index.match(ctx, strings.Repeat("safecontent", 32768))
	if !errors.Is(err, context.Canceled) || matches != nil {
		t.Fatalf("扫描中取消未生效：%v/%v", matches, err)
	}
}

func TestSensitivePlainIndexConcurrentReaders(t *testing.T) {
	words := sensitivePlainIndexFixture([]string{"he", "she", "hers", "水杉", "杉", "水杉"})
	index := mustSensitivePlainIndex(t, words)
	if index == nil {
		t.Fatal("没有建立普通规则索引")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				matches, err := index.match(t.Context(), "ushers水杉")
				if err != nil {
					t.Error(err)
					return
				}
				for i, word := range words {
					if matches.contains(i) != strings.Contains("ushers水杉", word.plain) {
						t.Errorf("并发扫描结果错误：%q", word.plain)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

func mustSensitivePlainIndex(t testing.TB, words []sensitiveWord) *sensitivePlainIndex {
	t.Helper()
	index, err := newSensitivePlainIndex(t.Context(), words)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func TestSensitivePlainIndexDiscardsHitsOnCancellation(t *testing.T) {
	words := sensitivePlainScanWords(minSensitivePlainRules, false)
	index := mustSensitivePlainIndex(t, words)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &sensitivePlainCancelContext{Context: parent, cancel: cancel, limit: 2}
	var patterns []string
	for _, word := range words {
		patterns = append(patterns, word.plain)
	}
	matches, err := index.match(ctx, strings.Join(patterns, " "))
	if !errors.Is(err, context.Canceled) || matches != nil {
		t.Fatalf("全部命中时取消必须丢弃部分结果：%v/%v", matches, err)
	}
}

func TestSensitivePlainIndexThresholdAndBuildCancellation(t *testing.T) {
	if index := mustSensitivePlainIndex(t, sensitivePlainScanWords(minSensitivePlainRules-1, false)); index != nil {
		t.Fatal("小规则集不应创建索引")
	}
	re, err := compileSensitiveRegex("regex")
	if err != nil {
		t.Fatal(err)
	}
	words := sensitivePlainScanWords(minSensitivePlainRules, false)
	for i := range words {
		words[i].re = re
	}
	if index := mustSensitivePlainIndex(t, words); index != nil {
		t.Fatal("正则规则不应参与普通词索引")
	}
	for _, limit := range []int{1, 3, 4} {
		parent, cancel := context.WithCancel(t.Context())
		ctx := &sensitivePlainCancelContext{Context: parent, cancel: cancel, limit: limit}
		index, err := newSensitivePlainIndex(ctx, sensitivePlainScanWords(minSensitivePlainRules, false))
		cancel()
		if !errors.Is(err, context.Canceled) || index != nil {
			t.Fatalf("构造取消检查 %d：%v/%v", limit, index, err)
		}
	}
}

func TestSensitivePlainIndexMatcherCompatibility(t *testing.T) {
	words := sensitivePlainScanWords(minSensitivePlainRules, false)
	for i, pattern := range []string{"加V", "好友", "😀水", "水杉", "杉", "好友", "s", "ſ"} {
		words[i].plain = foldSensitive(pattern, true)
		words[i].hit.Pattern = pattern
	}
	for _, pattern := range []string{`ＱＱ[0-9]{5,}`, `vx:\s*[a-z0-9_]{5,}`} {
		re, err := compileSensitiveRegex(pattern)
		if err != nil {
			t.Fatal(err)
		}
		words = append(words, sensitiveWord{hit: SensitiveHit{WordID: int64(len(words) + 1), Pattern: pattern, Level: SensitiveBlock}, re: re})
	}
	for _, preview := range []bool{true, false} {
		current, previous := cachedSensitiveService(words), cachedSensitiveService(words)
		previous.sensitive.plain = nil
		for _, text := range []string{"", "加Ｖ", strings.Repeat("safecontent", 4096), strings.Repeat("safecontent", 4096) + "加 Ｖ 好\u200b友 😀水杉 ＱＱ123456 VX: abc_12345 ſ", strings.Repeat("a", 4096) + "\xff好友"} {
			got, err := (sensitiveMatcher{a: current, preview: preview}).Match(t.Context(), text)
			want, wantErr := (sensitiveMatcher{a: previous, preview: preview}).Match(t.Context(), text)
			if err != nil || wantErr != nil || !slices.Equal(got, want) {
				t.Fatalf("匹配结果改变：%v/%v，原结果=%v/%v", got, err, want, wantErr)
			}
		}
		if !maps.Equal(current.sensitive.pending, previous.sensitive.pending) {
			t.Fatal("命中计数改变")
		}
	}
}

func TestSensitivePlainIndexCacheRefresh(t *testing.T) {
	db := testStore(t)
	if _, err := db.pool.Exec(t.Context(), `TRUNCATE admin_sensitive_words RESTART IDENTITY CASCADE;
 INSERT INTO admin_sensitive_words(pattern,category,level,created_by)
 SELECT 'rule'||to_char(n,'FM0000'),'custom',CASE WHEN n%2=0 THEN 'block' ELSE 'review' END,'test@example.test'
 FROM generate_series(0,127) AS n`); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: db}
	oldWords, oldIndex, err := service.sensitive.list(t.Context(), db, time.Now())
	if err != nil || oldIndex == nil {
		t.Fatalf("初始索引=%v/%v", oldIndex, err)
	}
	text := strings.Repeat("z", minSensitivePlainText) + "rule0000 replacement rule0001"
	hits, err := service.SensitivePreview().Match(t.Context(), text)
	if err != nil || len(hits) != 2 || hits[0].Pattern != "rule0000" || hits[1].Pattern != "rule0001" {
		t.Fatalf("初始命中=%v/%v", hits, err)
	}
	if _, err = db.pool.Exec(t.Context(), `DELETE FROM admin_sensitive_words WHERE pattern='rule0000';
 UPDATE admin_sensitive_words SET level='block' WHERE pattern='rule0001';
 INSERT INTO admin_sensitive_words(pattern,category,level,created_by) VALUES('replacement','custom','review','test@example.test')`); err != nil {
		t.Fatal(err)
	}
	service.sensitive.invalidate()
	hits, err = service.SensitivePreview().Match(t.Context(), text)
	if err != nil || len(hits) != 2 || hits[0].Pattern != "rule0001" || hits[1].Pattern != "replacement" {
		t.Fatalf("刷新后命中=%v/%v", hits, err)
	}
	oldMatches, err := oldIndex.match(t.Context(), text)
	if err != nil {
		t.Fatal(err)
	}
	for i, word := range oldWords {
		if oldMatches.contains(i) != strings.Contains(text, word.plain) {
			t.Fatal("刷新修改了仍被请求持有的旧快照")
		}
	}
	newWords, newIndex, err := service.sensitive.list(t.Context(), db, time.Now())
	if err != nil || newIndex == nil || newIndex == oldIndex {
		t.Fatalf("刷新后索引=%v/%v", newIndex, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	served, index, err := service.sensitive.list(ctx, db, time.Now().Add(2*sensitiveRefresh))
	if err != nil || index != newIndex || len(served) != len(newWords) || &served[0] != &newWords[0] {
		t.Fatalf("失败刷新没有保留同一快照：%v", err)
	}
}

func BenchmarkSensitivePlainIndexBuild(b *testing.B) {
	for _, count := range []int{128, 1024} {
		for _, shared := range []bool{true, false} {
			words := sensitivePlainScanWords(count, true)
			kind := "shared"
			if !shared {
				kind = "distinct"
				random := rand.New(rand.NewSource(702))
				for i := range words {
					var pattern strings.Builder
					for range 200 {
						pattern.WriteByte(byte('a' + random.Intn(26)))
					}
					words[i].plain = pattern.String()
				}
			}
			b.Run(fmt.Sprintf("%d/%s", count, kind), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					index, err := newSensitivePlainIndex(b.Context(), words)
					if err != nil || index == nil {
						b.Fatalf("构建索引：%v", err)
					}
					b.ReportMetric(float64(cap(index.nodes))*float64(unsafe.Sizeof(sensitivePlainNode{})), "node-B")
				}
			})
		}
	}
}
