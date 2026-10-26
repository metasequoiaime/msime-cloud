package server

import (
	"math/rand"
	"strings"
	"testing"
)

// previousQQReleaseNotes 保留完整拼接的基线，用于检查提前停止是否改变截句结果。
func previousQQReleaseNotes(body string) string {
	var b strings.Builder
	heading := false
	qqReleaseNotesVisibleSegments(body, func(segment string) {
		for line := range strings.SplitSeq(qqStripImagesAndHTML(segment), "\n") {
			trimmed := strings.TrimSpace(line)
			text := strings.TrimRight(qqPlainText(trimmed), "。；;：: ")
			if text == "" {
				continue
			}
			if b.Len() > 0 && !heading {
				b.WriteString("；")
			}
			b.WriteString(text)
			heading = strings.HasPrefix(trimmed, "#")
			if heading {
				b.WriteString("：")
			}
		}
	})
	return qqTruncateSentence(strings.TrimSuffix(b.String(), "："), qqReleaseNoteRunes)
}

func TestQQReleaseNotesPrefixMatchesPrevious(t *testing.T) {
	compare := func(body string) {
		t.Helper()
		if got, want := qqReleaseNotes(body), previousQQReleaseNotes(body); got != want {
			t.Fatalf("发布说明截断结果变化：输入=%q，结果=%q，预期=%q", body, got, want)
		}
	}
	for n := qqReleaseNoteRunes - 3; n <= qqReleaseNoteRunes+3; n++ {
		for _, unit := range []string{"甲", "a", "😀", "\xff"} {
			prefix := strings.Repeat(unit, n)
			for _, body := range []string{
				prefix, "# " + prefix, "# " + prefix + "\n后文", prefix + "\n# 标题\n正文",
				prefix + "；尾词\n后文", prefix + "\n![图](\nhttps://example.test/a.png\n)\n尾词",
				"# " + prefix + "\n```text\n代码\n```\n后文", prefix + "\n# 标题\n# 后续标题",
			} {
				compare(body)
			}
		}
	}
	rng := rand.New(rand.NewSource(72))
	parts := []string{"- **修复** `合成问题`\n", "# 标题\n", "\n", "![图](\nsynthetic\n)\n", "<b>正文</b>\n", "```text\n", "代码\n```\n", "甲；乙！丙。\n", "- [链接](https://example.test)\n", "\xff合成\xfe\n", " \u3000 \n"}
	for range 2000 {
		var body strings.Builder
		for n := rng.Intn(160); n > 0; n-- {
			body.WriteString(parts[rng.Intn(len(parts))])
		}
		compare(body.String())
	}
}

func TestQQReleaseNotesPrefixAllocationBudget(t *testing.T) {
	for _, separated := range []bool{false, true} {
		t.Run(map[bool]string{false: "continuous", true: "segments"}[separated], func(t *testing.T) {
			line := "- **修复** `合成问题`\n"
			if separated {
				line += "```text\n合成代码\n```\n"
			}
			body := strings.Repeat(line, 4096)
			want := previousQQReleaseNotes(body)
			var got string
			allocations := testing.AllocsPerRun(3, func() { got = qqReleaseNotes(body) })
			if got != want {
				t.Fatalf("长正文截句结果变化：%q，预期=%q", got, want)
			}
			// 只清洗最终摘要所需的行，预算不随被截掉的数千行增长。
			if allocations > 128 {
				t.Fatalf("发布说明分配 %.0f 次，超过 128 次预算", allocations)
			}
		})
	}
}

func BenchmarkQQReleaseNotesPrefix(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"short", "## 更新\n- 修复问题"},
		{"ordinary", strings.Repeat("- **修复** `合成问题`\n", 64)},
		{"long", strings.Repeat("- **修复** `合成问题`\n", 4096)},
		{"segments", strings.Repeat("- **修复** `合成问题`\n```text\n合成代码\n```\n", 4096)},
		{"single_line", strings.Repeat("合成正文", 16384)},
	} {
		for _, implementation := range []struct {
			name string
			fn   func(string) string
		}{
			{"previous", previousQQReleaseNotes}, {"bounded", qqReleaseNotes},
		} {
			b.Run(tc.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = implementation.fn(tc.body)
				}
			})
		}
	}
}
