package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestReleaseNotePrefixesMatchPrevious(t *testing.T) {
	check := func(body string) {
		t.Helper()
		if got, want := parseReleaseNotes(body), previousParseReleaseNotes(body); !reflect.DeepEqual(got, want) {
			t.Fatalf("发布说明解析变化：输入=%q，结果=%#v，期望=%#v", body, got, want)
		}
	}
	for first := range 256 {
		for _, rest := range []string{"", " 正文", "\t正文", "\v正文", "\u00a0正文", ". 正文", ") [x] 正文", "# 修复 ###"} {
			check("## 新增\n" + string([]byte{byte(first)}) + rest + "\n末尾正文")
		}
	}
	for _, line := range []string{"- [ ] 待处理", "* [X] 已处理", "+ [x] 已处理", "0. 正文", "123) 正文", "１. 正文", "١. 正文", "####### 修复", "##修复", "## 修复 ###", "## 其他", "  ### 改进\t", "<!--隐藏-->- 列表", "<!--隐藏-->## 修复"} {
		check("## 新增\n" + line + "\n末尾正文")
	}
}

func TestReleaseNotesPlainLinesAllocationBudget(t *testing.T) {
	body := strings.Repeat("正常说明\n", maxReleaseNoteLines)
	var got []releaseNote
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = parseReleaseNotes(body)
		}
	})
	if !reflect.DeepEqual(got, previousParseReleaseNotes(body)) {
		t.Fatal("普通说明的数量或正文变化")
	}
	// 说明正文不足 2 KiB，预算为结果切片和文本缓冲区的增长留出余量。
	if allocated := result.AllocedBytesPerOp(); allocated > 64<<10 {
		t.Fatalf("普通说明仍执行不可能匹配的正则：每次分配 %d 字节，超过 64 KiB 预算", allocated)
	}
}

func BenchmarkReleaseNotePrefixes(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"普通正文", strings.Repeat("正常说明\n", maxReleaseNoteLines)},
		{"数字正文", strings.Repeat("2026 年兼容性说明\n", maxReleaseNoteLines)},
		{"混合正文", strings.Repeat("## 修复\n- 修复输入问题\n普通说明\n", 40)},
		{"列表正文", strings.Repeat("## 修复\n1. 修复输入问题\n", maxReleaseNoteLines)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = parseReleaseNotes(tc.body)
			}
		})
	}
}
