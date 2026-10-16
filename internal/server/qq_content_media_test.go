package server

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestQQReleaseNotesVisibleSegments(t *testing.T) {
	input := "前![截图](\nhttps://example.test/a.png\n)\n```text\n<tag synthetic\n```\n后<b>尾词</b>"
	var got []string
	qqReleaseNotesVisibleSegments(input, func(segment string) {
		got = append(got, segment)
	})
	want := []string{
		"前![截图](\nhttps://example.test/a.png\n)\n",
		"后<b>尾词</b>",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("围栏外片段错误：结果=%q，预期=%q", got, want)
	}
}

func TestQQReleaseNotesMediaKeepsRules(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"plain", "甲 乙", "甲 乙"},
		{"image", "甲![截图](https://example.com/a.png)乙", "甲 乙"},
		{"empty_label", "甲![](synthetic)乙", "甲 乙"},
		{"html", "甲<b>乙</b>丙", "甲 乙 丙"},
		{"mixed", "甲<b>乙</b>![截图](synthetic)丙", "甲 乙 丙"},
		{"html_before_unclosed_image", "甲<b>乙</b> ![未闭合", "甲 乙 ![未闭合"},
		{"image_after_unclosed_html", "< 未闭合 ![截图](synthetic)", "< 未闭合"},
		{"multiline_image", "![截图](\nhttps://example.com/a.png\n)\n正文", "正文"},
		{"unclosed_image", "甲 ![截图] 乙", "甲 ![截图] 乙"},
		{"unclosed_html", "甲 < 乙", "甲 < 乙"},
		{"empty_html", "甲 <> 乙", "甲 <> 乙"},
		{"html_newline", "甲<tag\nx>乙", "甲<tag；x>乙"},
		{"comparison", "甲 < 乙 > 丙", "甲 丙"},
		{"invalid_utf8", "甲\xff<b>乙\xfe</b>丙", "甲\xff 乙\xfe 丙"},
		{"empty", "", ""},
		{"image_only", "![截图](synthetic)", ""},
		{"late_image", "甲 乙 丙 丁 戊 己 庚 辛![截图](x)", "甲 乙 丙 丁 戊 己 庚 辛"},
		{"late_html", "甲 乙 丙 丁 戊 己 庚 辛<b>尾词</b>", "甲 乙 丙 丁 戊 己 庚 辛 尾词"},
		{"late_html_single", "甲 乙 丙 丁 戊 己 庚 辛<br/>尾词", "甲 乙 丙 丁 戊 己 庚 辛 尾词"},
		{"late_invalid_prefix", "甲 乙 丙 丁 戊 己 庚 辛<> ![截图](x)尾词", "甲 乙 丙 丁 戊 己 庚 辛<> 尾词"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqReleaseNotes(tc.body); got != tc.want {
				t.Fatalf("图片和 HTML 过滤结果发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQReleaseNotesMediaAllocationBudget(t *testing.T) {
	code := "```text\n" + strings.Repeat("synthetic code\n", 8192) + "```\n"
	for _, tc := range []struct {
		name, body string
		budget     uint64
	}{
		{"plain", "```text\n" + strings.Repeat("synthetic code **value** `sample`\n", 8192) + "```\n正文", 64 << 10},
		{"unclosed_image", "```text\n" + strings.Repeat("![截图] incomplete\n", 8192) + "```\n正文", 64 << 10},
		{"unclosed_html", "```text\n" + strings.Repeat("<tag synthetic\n", 8192) + "```\n正文", 64 << 10},
		{"image_last", code + "![截图](synthetic)正文", 192 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := qqReleaseNotes(tc.body)
			runtime.ReadMemStats(&after)
			if got != "正文" {
				t.Fatalf("代码块影响了后续正文：%q", got)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("图片和 HTML 过滤分配 %d 字节", allocated)
			// 输入在测量前构造；预算排除无匹配的全文复制和晚出现匹配时对普通前缀的重复复制。
			if allocated > tc.budget {
				t.Fatalf("图片和 HTML 过滤分配 %d 字节，超过 %d 字节预算", allocated, tc.budget)
			}
		})
	}
}

func BenchmarkQQReleaseNotesMedia(b *testing.B) {
	code := "```text\n" + strings.Repeat("synthetic code\n", 8192) + "```\n"
	for _, tc := range []struct{ name, body string }{
		{"plain", code + "正文"},
		{"unclosed_image", "```text\n" + strings.Repeat("![截图] incomplete\n", 8192) + "```\n正文"},
		{"unclosed_html", "```text\n" + strings.Repeat("<tag synthetic\n", 8192) + "```\n正文"},
		{"image_first", "![截图](synthetic)\n" + code + "正文"},
		{"image_last", code + "![截图](synthetic)正文"},
		{"short_html", "甲<b>乙</b>丙"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqReleaseNotes(tc.body)
			}
		})
	}
}

func BenchmarkQQReleaseMaterialRepeated(b *testing.B) {
	for _, cached := range []bool{false, true} {
		b.Run(map[bool]string{false: "重新清洗", true: "复用缓存"}[cached], func(b *testing.B) {
			list := []qqPlatformRelease{{
				ID: "windows", Name: "Windows", Repo: "o/win",
				Stable: &qqRelease{TagName: "v0.10.1", PublishedAt: "2026-10-11T00:00:00Z", Body: "- " + strings.Repeat("合成更新内容 ", 2000)},
			}}
			b.ReportAllocs()
			for b.Loop() {
				list[0].Stable.notesReady = cached
				all := qqReleasesMaterial(list, nil)
				if !cached {
					list[0].Stable.notesReady = false
				}
				filtered := qqReleasesMaterial(list, func(qqPlatformRelease) bool { return true })
				if all == "" || filtered == "" {
					b.Fatal("版本资料为空")
				}
			}
		})
	}
}
