package server

import (
	"math/rand"
	"strings"
	"testing"
)

func TestQQPlainTextInlineMarkersMatchPrevious(t *testing.T) {
	previous := strings.NewReplacer("**", "", "__", "", "`", "")
	random := rand.New(rand.NewSource(23))
	units := []string{"水", "😀", " ", "\t", "\u3000", "*", "_", "`", "\xff", "\xe4\xb8"}
	for range 1000 {
		var input strings.Builder
		// 固定普通正文前缀，单独验证行内替换，不触发行首标记或代码围栏。
		input.WriteString("正文 ")
		for range random.Intn(200) {
			input.WriteString(units[random.Intn(len(units))])
		}
		text := input.String()
		want := strings.Join(strings.Fields(previous.Replace(text)), " ")
		if got := qqPlainText(text); got != want {
			t.Fatalf("行内标记替换不兼容：输入=%q，结果=%q，预期=%q", text, got, want)
		}
	}
}

func TestQQPlainInlineTextMatchesPrevious(t *testing.T) {
	previous := strings.NewReplacer("**", "", "__", "", "`", "")
	units := []string{"", "*", "_", "`", "***", "___", "*`*", "_`_", "水", "\xff", "\n"}
	for _, a := range units {
		for _, b := range units {
			for _, c := range units {
				text := a + b + c
				if got, want := qqPlainInlineText(text), previous.Replace(text); got != want {
					t.Fatalf("行内删除规则不兼容：输入=%q，结果=%q，预期=%q", text, got, want)
				}
			}
		}
	}
}

func TestQQPlainTextInlineMarkers(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "普通正文", "普通正文"},
		{"markers", "**加粗** __强调__ `代码`", "加粗 强调 代码"},
		{"overlapping", "***加粗*** ___强调___ ``代码``", "*加粗* _强调_ 代码"},
		{"adjacent", "**__前半__****后半**", "前半后半"},
		{"single", "星*号 下_划线", "星*号 下_划线"},
		{"link", "[**链接**](https://example.com) __后文__", "链接 后文"},
		{"invalid_utf8", "**前\xff后** `尾\xfe`", "前\xff后 尾\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for range 32 {
				if got := qqPlainText(tc.input); got != tc.want {
					t.Fatalf("行内标记转换发生变化：结果=%q，预期=%q", got, tc.want)
				}
			}
		})
	}
}

func TestQQPlainTextShortAllocationBudget(t *testing.T) {
	var got string
	allocations := testing.AllocsPerRun(100, func() {
		got = qqPlainText("普通正文")
	})
	if got != "普通正文" {
		t.Fatalf("短正文转换发生变化：%q", got)
	}
	t.Logf("短正文转换分配 %.1f 次", allocations)
	// 预算包含 race 下正则对象池的额外分配，仍排除每次重建固定规则的开销。
	if allocations > 23 {
		t.Fatalf("短正文转换分配 %.1f 次，超过 23 次预算", allocations)
	}
}

func BenchmarkQQPlainTextReplacer(b *testing.B) {
	for _, tc := range []struct{ name, input string }{
		{"plain", "普通正文"},
		{"formatted", "**加粗** __强调__ `代码`"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqPlainText(tc.input)
			}
		})
	}
	b.Run("release_lines", func(b *testing.B) {
		body := strings.Repeat("- **修复** `合成问题`\n", 64)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_ = qqReleaseNotes(body)
		}
	})
}
