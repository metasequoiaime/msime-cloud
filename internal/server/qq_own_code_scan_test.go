package server

import (
	"context"
	"math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 固定原来的正则及去重规则，核对边界消耗、编号顺序和三个编号的上限。
var previousQQOwnCodePattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])D\s?([0-9]{1,6})(?:[^A-Za-z0-9]|$)`)

func previousQQOwnCodeIDs(question string) []int64 {
	var ids []int64
	for _, match := range previousQQOwnCodePattern.FindAllStringSubmatch(question, -1) {
		id, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || id <= 0 || slices.Contains(ids, id) || len(ids) == 3 {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// 记录真正查询的编号，返回不存在，避免格式化草稿状态掩盖解析差异。
type qqOwnCodeLookupStore struct {
	memoryDrafts
	ids []int64
}

func (s *qqOwnCodeLookupStore) QQDraft(_ context.Context, id int64) (account.QQDraft, error) {
	s.ids = append(s.ids, id)
	return account.QQDraft{}, account.ErrQQDraftNotFound
}

func assertQQOwnCodeLookups(t *testing.T, question string, want []int64) {
	t.Helper()
	store := &qqOwnCodeLookupStore{}
	s := &Server{qqDrafts: store}
	got := s.qqOwnCodes(context.Background(), question)
	if !slices.Equal(store.ids, want) || (got == "") != (len(want) == 0) {
		t.Fatalf("草稿查询=%v，期望 %v；资料为空=%v；输入=%q", store.ids, want, got == "", question)
	}
}

func TestQQOwnCodeScanCompatibility(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []int64
	}{
		{"", nil},
		{"普通问题、3D、AD3、D3D11", nil},
		{"D1 D2 D3 D4 D5", []int64{1, 3, 5}},
		{"D0 D1 D2 D3 D4", []int64{2, 4}},
		{"D1， D1 D2 D3 D4", []int64{1, 3}},
		{"D1\xffD2\xfeD3", []int64{1, 3}},
		{"D1， D2， D3， D4", []int64{1, 2, 3}},
		{"d 000001， D1， D000000， D000002， D3", []int64{1, 2, 3}},
		{"D1234567， D123456， D1A， D2_， D3-", []int64{123456, 2, 3}},
		{"D\t1， D\n2， D\r3", []int64{1, 2, 3}},
		{"D\f1， D\v2， D\u00a03， D  4， D\u30005", []int64{1}},
		{"ſD1， KD2， D3ſ， D4K， éD5é， D６， D7６", []int64{5, 7}},
		{"\xffD1\xfe D2😀 D3\xe4\xb8 D4", []int64{1, 2, 3}},
		{strings.Repeat("D0， ", 4096) + "D1， D2， D3", []int64{1, 2, 3}},
		{strings.Repeat("D1， ", 4096) + "D2， D3", []int64{1, 2, 3}},
	} {
		if got := previousQQOwnCodeIDs(tc.text); !slices.Equal(got, tc.want) {
			t.Fatalf("旧实现基准=%v，期望 %v；输入=%q", got, tc.want, tc.text)
		}
		assertQQOwnCodeLookups(t, tc.text, tc.want)
	}
	random := rand.New(rand.NewSource(73))
	units := []string{"D", "d", "D0", "D1", "D000001", "D123456", "D1234567", "d 2", "A", "Z", "a", "3", "ſ", "K", "é", "６", "中", "😀", " ", "\t", "\n", "\r", "\f", "\v", "\u00a0", "\u3000", "\xff", "\xe4\xb8", "，", "_", "-"}
	for range 3000 {
		var question strings.Builder
		for range random.Intn(64) {
			question.WriteString(units[random.Intn(len(units))])
		}
		text := question.String()
		assertQQOwnCodeLookups(t, text, previousQQOwnCodeIDs(text))
	}
}

func TestQQOwnCodeScanAllocationBudget(t *testing.T) {
	s := &Server{qqDrafts: &memoryDrafts{}}
	ctx := context.Background()
	want := s.qqOwnCodes(ctx, "D1， D2， D3。")
	for _, tc := range []struct{ name, question string }{
		{"early_limit", "D1， D2， D3。" + strings.Repeat(" D4。", 4096)},
		{"duplicates", strings.Repeat("D1， ", 4096) + "D2， D3。"},
		{"zeros", strings.Repeat("D0， ", 4096) + "D1， D2， D3。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					got = s.qqOwnCodes(ctx, tc.question)
				}
			})
			if got != want {
				t.Fatalf("草稿资料变化：结果=%q，期望 %q", got, want)
			}
			t.Logf("%s：%s，%s", tc.name, result.String(), result.MemString())
			if result.AllocedBytesPerOp() > 4096 {
				t.Fatalf("最多三个草稿编号时分配 %d 字节，超过 4096 字节预算", result.AllocedBytesPerOp())
			}
		})
	}
}

func BenchmarkQQOwnCodeIDs(b *testing.B) {
	for _, tc := range []struct{ name, question string }{
		{"early_limit", "D1， D2， D3。" + strings.Repeat(" D4。", 4096)},
		{"duplicates", strings.Repeat("D1， ", 4096) + "D2， D3。"},
		{"zeros", strings.Repeat("D0， ", 4096) + "D1， D2， D3。"},
		{"no_matches", strings.Repeat("普通问题，AD3 和 D3D11 不是草稿。", 4096)},
	} {
		want := previousQQOwnCodeIDs(tc.question)
		for _, implementation := range []struct {
			name string
			run  func(string) []int64
		}{
			{"previous", previousQQOwnCodeIDs},
			{"current", qqOwnCodeIDs},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				var got []int64
				for b.Loop() {
					got = implementation.run(tc.question)
				}
				if !slices.Equal(got, want) {
					b.Fatalf("草稿编号=%v，期望 %v", got, want)
				}
			})
		}
	}
}
