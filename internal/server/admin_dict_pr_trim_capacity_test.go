package server

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestTrimDictionaryContentSmallOutputAllocations(t *testing.T) {
	large := strings.Repeat("合成词条", 65536)
	for _, tc := range []struct {
		name, content, want string
		at                  int
	}{
		{"全部删除", large, "", 0},
		{"仅余尾部空段", large + "\n", "", 0},
		{"仅余短词条", large + "\n保留\tkeep\n", "保留\tkeep\n", 0},
		{"删除长尾行", "保留\tkeep\n" + large, "保留\tkeep", 1},
		{"删除长尾行保留换行", "保留\tkeep\n" + large + "\n", "保留\tkeep\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drop := map[int]bool{tc.at: true}
			var got string
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = trimDictionaryContent(tc.content, drop)
				}
			})
			if got != tc.want {
				t.Fatalf("裁剪结果长度=%d，期望 %d", len(got), len(tc.want))
			}
			if result.AllocedBytesPerOp() > 4096 || (tc.want == "" && result.AllocsPerOp() != 0) {
				t.Fatalf("裁剪后仅 %d 字节仍分配 %s", len(got), result.MemString())
			}
		})
	}
}

func TestTrimDictionaryContentRandomDeletions(t *testing.T) {
	random := rand.New(rand.NewSource(630))
	units := []string{"词条", "\t", "\r", "\n", "", "# 注释", "\xff", "\xe4\xb8", "\x00"}
	for range 1000 {
		var content strings.Builder
		for range random.Intn(80) {
			content.WriteString(units[random.Intn(len(units))])
		}
		text := content.String()
		lines := strings.Count(text, "\n") + 1
		drop := map[int]bool{}
		for i := -1; i <= lines+1; i++ {
			switch random.Intn(3) {
			case 0:
				drop[i] = true
			case 1:
				drop[i] = false
			}
		}
		before := make(map[int]bool, len(drop))
		for at, removed := range drop {
			before[at] = removed
		}
		got, want := trimDictionaryContent(text, drop), previousTrimDictionaryContent(text, drop)
		if got != want || !reflect.DeepEqual(drop, before) {
			t.Fatalf("输入=%q，删除=%v，结果=%q，期望=%q", text, before, got, want)
		}
	}
}

func TestTrimDictionaryContentManyKeptSegments(t *testing.T) {
	for _, segmentCount := range []int{15, 16, 17, 100} {
		for _, kept := range []string{"保留\r", ""} {
			content := strings.Repeat(kept+"\n删除\n", segmentCount)
			drop := map[int]bool{}
			for i := range segmentCount {
				drop[i*2+1] = true
			}
			got, want := trimDictionaryContent(content, drop), previousTrimDictionaryContent(content, drop)
			if got != want {
				t.Fatalf("保留片段 %d 个，结果=%q，期望=%q", segmentCount+1, got, want)
			}
		}
	}
}

func BenchmarkTrimDictionaryContentCapacity(b *testing.B) {
	large := strings.Repeat("合成词条", 65536)
	rows := strings.Repeat("合成词条\tci'tiao\t1\n", 50000)
	for _, tc := range []struct{ name, content string }{
		{"全部删除", large},
		{"仅余短词条", large + "\n保留\tkeep\n"},
		{"删除一行保留五万行", rows},
	} {
		b.Run(tc.name, func(b *testing.B) {
			drop := map[int]bool{0: true}
			b.ReportAllocs()
			for b.Loop() {
				_ = trimDictionaryContent(tc.content, drop)
			}
		})
	}
}
