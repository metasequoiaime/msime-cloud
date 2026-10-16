package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestQQReleaseNotesFenceTransitions(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"closed", "## 更新\n```go\n# 不应出现\n**合成代码**\n```\n- 修复问题\n", "更新：修复问题"},
		{"unclosed", "- 保留内容\n```\n## 不应出现\n尾部代码", "保留内容"},
		{"code_only", "```\n**合成代码**\n```", ""},
		{"indented", "\u3000```go\r\n\t**合成代码**\r\n\u00a0```\r\n- 后续内容", "后续内容"},
		{"multiple", "- 前文\n```\n代码一\n```\n## 新增\n```text\n代码二\n```\n- 后文", "前文；新增：后文"},
		{"prefix", "```go\n代码一\n````\n- 后文", "后文"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqReleaseNotes(tc.body); got != tc.want {
				t.Fatalf("发布说明代码块过滤发生变化：结果=%q，预期=%q", got, tc.want)
			}
		})
	}
}

func TestQQReleaseNotesFencedCodeAllocationBudget(t *testing.T) {
	body := "## 更新\n```text\n" + strings.Repeat("synthetic code **value** `sample`\n", 4096) + "```\n- 修复问题"
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := qqReleaseNotes(body)
	runtime.ReadMemStats(&after)
	if got != "更新：修复问题" {
		t.Fatalf("长代码块影响了后续发布说明：%q", got)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("发布说明解析分配 %d 字节", allocated)
	// 预算包含整份输入的图片过滤及运行时开销，不允许为将被丢弃的每行代码转换纯文本。
	if allocated > 1<<20 {
		t.Fatalf("发布说明解析分配 %d 字节，超过 1 MiB 预算", allocated)
	}
}

func BenchmarkQQReleaseNotesFencedCode(b *testing.B) {
	code := strings.Repeat("synthetic code **value** `sample`\n", 4096)
	for _, tc := range []struct{ name, body string }{
		{"code_only", "```text\n" + code + "```"},
		{"mixed", "## 更新\n```text\n" + code + "```\n- 修复问题"},
		{"ordinary", strings.Repeat("- **修复** `合成问题`\n", 64)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqReleaseNotes(tc.body)
			}
		})
	}
}
