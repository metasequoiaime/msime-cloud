package server

import (
	"math/rand"
	"strings"
	"testing"
)

func TestQQTruncatePreservesRuneBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		limit      int
		want       string
	}{
		{"空文本", "", 0, ""},
		{"零上限", "水杉", 0, "…"},
		{"短文本", "水杉", 3, "水杉"},
		{"正好达到上限", "水杉", 2, "水杉"},
		{"英文截断", "water", 3, "wat…"},
		{"中文截断", "水杉输入法", 2, "水杉…"},
		{"四字节字符", "水😀杉", 2, "水😀…"},
		{"组合标记独立计数", "a\u0301b", 1, "a…"},
		{"空字节", "a\x00b", 2, "a\x00…"},
		{"替换符本身是有效字符", "�水杉", 2, "�水…"},
		{"未截断时保留无效字节", "水\xff", 2, "水\xff"},
		{"截断时逐字节替换无效前缀", "\xff\xfeABC", 2, "��…"},
		{"混合无效字节", "水\xff杉", 2, "水�…"},
		{"截断的多字节序列", "\xe4\xb8", 1, "�…"},
		{"代理区编码", "\xed\xa0\x80x", 2, "��…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqTruncate(tc.text, tc.limit); got != tc.want {
				t.Fatalf("qqTruncate(%q,%d)=%q，期望 %q", tc.text, tc.limit, got, tc.want)
			}
		})
	}
}

func TestQQTruncateMatchesPreviousEncodingSemantics(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for size := 0; size < 128; size++ {
		data := make([]byte, size)
		if _, err := random.Read(data); err != nil {
			t.Fatal(err)
		}
		text := string(data)
		runes := []rune(text)
		for limit := 0; limit <= len(runes)+1; limit++ {
			want := text
			if len(runes) > limit {
				want = string(runes[:limit]) + "…"
			}
			if got := qqTruncate(text, limit); got != want {
				t.Fatalf("随机字节兼容性不一致：text=%q，limit=%d，结果=%q，期望=%q", text, limit, got, want)
			}
		}
	}
}

func TestQQTruncateRejectsNegativeLimit(t *testing.T) {
	for _, text := range []string{"", "水杉"} {
		t.Run(text, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("负数上限应保持原有 panic 行为")
				}
			}()
			qqTruncate(text, -1)
		})
	}
}

func TestQQTruncateAvoidsFullRuneAllocation(t *testing.T) {
	for _, unit := range []string{"a", "水", "😀"} {
		t.Run(unit, func(t *testing.T) {
			text := strings.Repeat(unit, 65536)
			want := strings.Repeat(unit, qqHistorySummaryRunes) + "…"
			var got string
			allocations := testing.AllocsPerRun(10, func() {
				got = qqTruncate(text, qqHistorySummaryRunes)
			})
			if got != want {
				t.Fatal("长文本摘要不正确")
			}
			if allocations > 1 {
				t.Fatalf("摘要分配 %.0f 次，期望只为结果分配一次，避免复制全文 rune 切片", allocations)
			}
		})
	}
}

func BenchmarkQQTruncate(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		limit      int
	}{
		{"short", "水杉输入法", qqHistorySummaryRunes},
		{"exact", strings.Repeat("水", qqHistorySummaryRunes), qqHistorySummaryRunes},
		{"long_ascii", strings.Repeat("a", 65536), qqHistorySummaryRunes},
		{"long_chinese", strings.Repeat("水", 65536), qqHistorySummaryRunes},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var got string
			for b.Loop() {
				got = qqTruncate(tc.text, tc.limit)
			}
			if got == "" {
				b.Fatal("摘要为空")
			}
		})
	}
}
