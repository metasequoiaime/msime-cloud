package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

func validationUTF16Command(text string) (map[string]any, pluginManifest) {
	trigger, title := "sample", "示例"
	rows := []struct {
		Trigger  *string `toml:"trigger"`
		Title    *string `toml:"title"`
		Template *string `toml:"template"`
	}{{&trigger, &title, &text}}
	table := map[string]any{"commands": []any{map[string]any{"trigger": trigger, "title": title, "template": text}}}
	return table, pluginManifest{Commands: &rows}
}

func TestValidationUTF16Boundaries(t *testing.T) {
	a := &Service{}
	for _, tc := range []struct {
		name, text                    string
		quick, short, template, table bool
	}{
		{"ASCII上限", strings.Repeat("a", 199), true, true, true, true},
		{"ASCII超限", strings.Repeat("a", 200), false, false, false, false},
		{"汉字上限", strings.Repeat("字", 199), true, true, true, true},
		{"汉字超限", strings.Repeat("字", 200), false, false, false, false},
		{"补充平面上限", strings.Repeat("😀", 99) + "a", true, true, true, true},
		{"补充平面超限", strings.Repeat("😀", 100), false, false, false, false},
		{"混合上限", strings.Repeat("字😀", 66) + "a", true, true, true, true},
		{"混合超限", strings.Repeat("字😀", 66) + "ab", false, false, false, false},
		{"空字符串", "", false, false, true, false},
		{"空白", " ", false, false, true, false},
		{"控制字符", "a\nb", false, false, false, false},
		{"无效UTF8", "\xff", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := engine.ErrInvalid
			if tc.quick {
				want = engine.ErrUnavailable
			}
			_, err := a.validateDictionary(t.Context(), "quick", []DictionaryEntry{{Word: tc.text, Weight: 1}})
			if !errors.Is(err, want) {
				t.Errorf("快捷词条错误=%v，期望 %v", err, want)
			}
			if got := pluginShortText(tc.text, 199); got != tc.short {
				t.Errorf("插件短文本=%t，期望 %t", got, tc.short)
			}
			if got := validCommandTemplate(tc.text); got != tc.template {
				t.Errorf("模板展开校验=%t，期望 %t", got, tc.template)
			}
			table, manifest := validationUTF16Command(tc.text)
			if got := validPluginCommands(table, manifest); got != tc.table {
				t.Errorf("命令表校验=%t，期望 %t", got, tc.table)
			}
		})
	}
	for _, kind := range []string{"pinyin", "wubi", "wubi98", "english"} {
		_, err := a.validateDictionary(t.Context(), kind, []DictionaryEntry{{Word: strings.Repeat("字", 200), Weight: 1}})
		if !errors.Is(err, engine.ErrUnavailable) {
			t.Errorf("%s 不应采用快捷词条长度上限：%v", kind, err)
		}
	}
}

func TestValidationUTF16Allocations(t *testing.T) {
	a := &Service{}
	ctx := context.Background()
	oversize := []DictionaryEntry{{Word: strings.Repeat("字", 200), Weight: 1}}
	text := strings.Repeat("字", 199)
	table, manifest := validationUTF16Command(oversize[0].Word)
	for _, tc := range []struct {
		name  string
		check func() bool
		max   float64
	}{
		{"快捷词条超限", func() bool {
			_, err := a.validateDictionary(ctx, "quick", oversize)
			return errors.Is(err, engine.ErrInvalid)
		}, 0},
		{"插件短文本", func() bool { return pluginShortText(text, 199) }, 0},
		{"命令表超限", func() bool { return !validPluginCommands(table, manifest) }, 0},
		// 两个时刻各保留一次展开字符串分配，不再为计数构造编码切片。
		{"模板展开", func() bool { return validCommandTemplate(text) }, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var valid bool
			allocations := testing.AllocsPerRun(1000, func() { valid = tc.check() })
			if !valid {
				t.Fatal("校验结果不符合预期")
			}
			if allocations > tc.max {
				t.Fatalf("UTF-16 长度校验分配 %g 次，预算 %g 次", allocations, tc.max)
			}
		})
	}
}

func BenchmarkValidationUTF16(b *testing.B) {
	a := &Service{}
	ctx := context.Background()
	text := strings.Repeat("字", 199)
	entries := []DictionaryEntry{{Word: text, Weight: 1}}
	table, manifest := validationUTF16Command(text)
	for _, tc := range []struct {
		name  string
		check func() bool
	}{
		{"快捷词条", func() bool {
			_, err := a.validateDictionary(ctx, "quick", entries)
			return errors.Is(err, engine.ErrUnavailable)
		}},
		{"插件短文本", func() bool { return pluginShortText(text, 199) }},
		{"命令表", func() bool { return validPluginCommands(table, manifest) }},
		{"模板展开", func() bool { return validCommandTemplate(text) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !tc.check() {
					b.Fatal("校验结果不符合预期")
				}
			}
		})
	}
}
