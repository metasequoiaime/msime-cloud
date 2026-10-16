package account

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 冻结完整去重流程，沿用当前提示格式化函数，只比较命中收集这一步的行为和成本。
func previousScreenCommunityHits(ctx context.Context, matcher SensitiveMatcher, texts ...string) (bool, *string, error) {
	hits, err := matcher.Match(ctx, strings.Join(texts, "\n"))
	if err != nil {
		return false, nil, err
	}
	var review []string
	for _, hit := range hits {
		switch hit.Level {
		case SensitiveBlock:
			return true, nil, nil
		case SensitiveReview:
			if !slices.Contains(review, hit.Pattern) {
				review = append(review, hit.Pattern)
			}
		}
	}
	if len(review) == 0 {
		return false, nil, nil
	}
	text := sensitiveFlagText(review)
	return false, &text, nil
}

func syntheticSensitivePrefixHits(count int) []SensitiveHit {
	hits := make([]SensitiveHit, count)
	for i := range hits {
		hits[i] = SensitiveHit{Pattern: "合成规则" + strings.Repeat("水", 190) + strconv.Itoa(i), Level: SensitiveReview}
	}
	return hits
}

func TestSensitiveHitPrefixCompatibility(t *testing.T) {
	check := func(hits []SensitiveHit, matchErr error) {
		t.Helper()
		matcher := &fakeMatcher{hits: hits, err: matchErr}
		blocked, flag, err := screenCommunityText(context.Background(), matcher, "合成内容", "第二段")
		wantBlocked, wantFlag, wantErr := previousScreenCommunityHits(context.Background(), matcher, "合成内容", "第二段")
		if blocked != wantBlocked || err != wantErr || (flag == nil) != (wantFlag == nil) || flag != nil && *flag != *wantFlag {
			t.Fatalf("拦截=%t，提示=%v，错误=%v；原拦截=%t，原提示=%v，原错误=%v", blocked, flag, err, wantBlocked, wantFlag, wantErr)
		}
	}
	check(nil, nil)
	check(nil, errors.New("synthetic matcher error"))
	for _, patterns := range [][]string{
		{"", "", "合成词", "合成词"},
		{strings.Repeat("水", 492), strings.Repeat("水", 492)},
		{strings.Repeat("水", 492), ""},
		{strings.Repeat("水", 490) + "\xff\xfe", strings.Repeat("水", 490) + "\xff\xfe"},
		{strings.Repeat("水", 490) + "\xff\xfe", "尾"},
		{strings.Repeat("😀", 1000), "后续规则"},
	} {
		var hits []SensitiveHit
		for _, pattern := range patterns {
			hits = append(hits, SensitiveHit{Pattern: pattern, Level: SensitiveReview})
		}
		check(hits, nil)
	}
	random := rand.New(rand.NewSource(696))
	atoms := []string{"a", "水", "😀", "\ufffd", "\xff", "\xfe", "\xf0\x9f", "\x00", "\t", "\n", "\u3000", "」「"}
	for range 1000 {
		hits := make([]SensitiveHit, random.Intn(200))
		for i := range hits {
			if i > 0 && random.Intn(4) == 0 {
				hits[i] = hits[random.Intn(i)]
				continue
			}
			var pattern strings.Builder
			for range random.Intn(100) {
				pattern.WriteString(atoms[random.Intn(len(atoms))])
			}
			hits[i] = SensitiveHit{Pattern: pattern.String(), Level: SensitiveReview}
			if random.Intn(5) == 0 {
				hits[i].Level = "unknown"
			}
		}
		check(hits, nil)
		check(append(hits, SensitiveHit{Pattern: "合成拦截规则", Level: SensitiveBlock}), nil)
	}
}

func TestSensitiveHitPrefixAllocationBudget(t *testing.T) {
	for _, hits := range [][]SensitiveHit{
		{{Pattern: "合成词", Level: SensitiveReview}},
		{{Pattern: "合成词", Level: SensitiveReview}, {Pattern: "合成词", Level: SensitiveReview}},
	} {
		matcher := &fakeMatcher{hits: hits}
		// race 编译会改变短切片的分配方式，和同一构建下的原流程比较。
		previousAllocs := testing.AllocsPerRun(100, func() {
			sensitiveHitPrefixBlocked, sensitiveHitPrefixFlag, _ = previousScreenCommunityHits(context.Background(), matcher, "合成内容")
		})
		allocs := testing.AllocsPerRun(100, func() {
			sensitiveHitPrefixBlocked, sensitiveHitPrefixFlag, _ = screenCommunityText(context.Background(), matcher, "合成内容")
		})
		if allocs > previousAllocs {
			t.Fatalf("%d 条短命中的去重及提示分配=%.0f，原流程=%.0f", len(hits), allocs, previousAllocs)
		}
	}
	hits := syntheticSensitivePrefixHits(1000)
	matcher := &fakeMatcher{hits: hits}
	_, want, _ := previousScreenCommunityHits(context.Background(), matcher, "合成内容")
	allocs := testing.AllocsPerRun(10, func() {
		blocked, flag, err := screenCommunityText(context.Background(), matcher, "合成内容")
		if blocked || err != nil || flag == nil || *flag != *want {
			t.Fatalf("拦截=%t，提示=%v，错误=%v", blocked, flag, err)
		}
	})
	if allocs > 5 {
		t.Fatalf("1000 条命中的去重及提示分配=%.0f，期望最多 5 次", allocs)
	}
	matcher.hits = append(hits, SensitiveHit{Pattern: "合成拦截规则", Level: SensitiveBlock})
	if blocked, flag, err := screenCommunityText(context.Background(), matcher, "合成内容"); !blocked || flag != nil || err != nil {
		t.Fatalf("大量复核命中之后的拦截规则被忽略：拦截=%t，提示=%v，错误=%v", blocked, flag, err)
	}
}

func TestSensitiveHitPrefixRestoredFlag(t *testing.T) {
	db, a, _, _, call := moderationFixture(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE admin_sensitive_words,admin_sensitive_hits`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.pool.Exec(context.Background(), `TRUNCATE admin_sensitive_words,admin_sensitive_hits`)
	})
	patterns := make([]string, 300)
	for i := range patterns {
		patterns[i] = "加班|合成规则" + strconv.Itoa(i)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO admin_sensitive_words(pattern,is_regex,category,level,created_by)
SELECT pattern,true,'custom','review','test' FROM unnest($1::text[]) WITH ORDINALITY AS v(pattern,n) ORDER BY n`, patterns); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE community_resources SET moderation='removed',previous_moderation='pending',moderation_reason='合成移除原因' WHERE id='reply-a'`); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/api/actions", `{"action":"restore_content","section":"replies","id":"reply-a"}`); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	state, previous, reason, _ := moderationState(t, db, "community_resources", "reply-a")
	if want := previousSensitiveFlagText(patterns); state != "pending" || previous != nil || reason == nil || *reason != want {
		t.Fatalf("恢复状态=%q，原状态=%v，提示=%v，期望提示=%q", state, previous, reason, want)
	}
	a.sensitive.mu.Lock()
	pending := len(a.sensitive.pending)
	a.sensitive.mu.Unlock()
	if pending != 0 {
		t.Fatalf("恢复重审记录了 %d 条提交命中统计，期望不记录", pending)
	}
}

var sensitiveHitPrefixFlag *string
var sensitiveHitPrefixBlocked bool

func BenchmarkSensitiveHitPrefix(b *testing.B) {
	for _, tc := range []struct {
		name string
		hits []SensitiveHit
	}{
		{"Empty", nil},
		{"Single", []SensitiveHit{{Pattern: "合成词", Level: SensitiveReview}}},
		{"Duplicates", []SensitiveHit{{Pattern: "合成词", Level: SensitiveReview}, {Pattern: "合成词", Level: SensitiveReview}}},
		{"Unique100", syntheticSensitivePrefixHits(100)},
		{"Unique1000", syntheticSensitivePrefixHits(1000)},
		{"LateBlock1000", append(syntheticSensitivePrefixHits(1000), SensitiveHit{Pattern: "合成拦截规则", Level: SensitiveBlock})},
	} {
		for _, impl := range []struct {
			name   string
			screen func(context.Context, SensitiveMatcher, ...string) (bool, *string, error)
		}{{"Previous", previousScreenCommunityHits}, {"Current", screenCommunityText}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				matcher := &fakeMatcher{hits: tc.hits}
				texts := []string{"合成内容"}
				b.ReportAllocs()
				for b.Loop() {
					var err error
					sensitiveHitPrefixBlocked, sensitiveHitPrefixFlag, err = impl.screen(context.Background(), matcher, texts...)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
