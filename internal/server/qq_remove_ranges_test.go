package server

import (
	"math/rand"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestQQRemoveLinkNotesPreservesUnmatchedBytes(t *testing.T) {
	previous := regexp.MustCompile(`（[A-Za-z][A-Za-z0-9+.-]*://[^）]*）`)
	check := func(input string) {
		t.Helper()
		want := previous.ReplaceAllString(input, "")
		if got := qqRemoveLinkNotes(input); got != want {
			t.Fatalf("链接注释删除变化：输入=%q，实际=%q，预期=%q", input, got, want)
		}
	}
	for _, input := range []string{
		"", "没有链接\t \r\n", "（https://example.test）", "前\t（HTTP+unix://socket/path）  后\r\n",
		"（a://）", "（7://无效）", "（普通://网址）", "（https://未闭合", "（不是链接）",
		"（https://a.test\n多行）后", "（https://a.test\xff）", "（无效（ftp://a.test）保留）",
		"（https://a.test）（ftp://b.test）", "https://example.test", "(https://example.test)",
	} {
		check(input)
	}
	units := []string{"", "（", "）", "(", ")", "a", "1", "+", "-", ".", ":", "/", "!", " ", "甲", "\n", "\t", "\xff", "https", "ftp", "://"}
	random := rand.New(rand.NewSource(1252))
	for range 5000 {
		var input strings.Builder
		for range random.Intn(100) {
			input.WriteString(units[random.Intn(len(units))])
		}
		check(input.String())
	}
}

func TestQQRemoveMatchedRangesPreservesWhitespaceAndAllocationBudget(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(string) string
		link string
	}{
		{"裸链接", qqRemoveRawLinks, "https://example.test/path"},
		{"链接注释", qqRemoveLinkNotes, "（https://example.test/path）"},
	} {
		for _, tc := range []struct {
			name, input, want string
			maxAllocs         float64
			maxBytes          int64
		}{
			{"没有匹配", "前\t \r\n后\xff", "前\t \r\n后\xff", 0, 0},
			{"全部删除", operation.link, "", 0, 0},
			{"相邻匹配", operation.link + "\t" + operation.link, "\t", 1, 16},
			{"保留空白", "前\t " + operation.link + " \r\n后", "前\t  \r\n后", 1, 32},
			{"长目标短结果", "前\t " + strings.Replace(operation.link, "path", strings.Repeat("x", 1<<20), 1) + " \r\n后", "前\t  \r\n后", 1, 32},
		} {
			t.Run(operation.name+"/"+tc.name, func(t *testing.T) {
				var got string
				allocs := testing.AllocsPerRun(20, func() { got = operation.run(tc.input) })
				if got != tc.want || allocs > tc.maxAllocs {
					t.Fatalf("结果=%q，预期=%q，分配=%.1f，预算=%.1f", got, tc.want, allocs, tc.maxAllocs)
				}
				if tc.maxBytes > 0 {
					const runs = 20
					var before, after runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&before)
					for range runs {
						got = operation.run(tc.input)
					}
					runtime.ReadMemStats(&after)
					allocated := (after.TotalAlloc - before.TotalAlloc) / runs
					if int64(allocated) > tc.maxBytes {
						t.Fatalf("分配 %d 字节，超过 %d 字节预算", allocated, tc.maxBytes)
					}
				}
			})
		}
	}
}
