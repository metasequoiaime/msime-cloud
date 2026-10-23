package server

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var previousQQCommandPolite = regexp.MustCompile(`^(?:帮我|帮忙|请|麻烦你?|我想|我要|想)\s*`)

func referenceQQPoliteCommandArg(text string, words ...string) (content string, glued, ok bool) {
	trimmed := previousQQCommandPolite.ReplaceAllString(strings.TrimSpace(text), "")
	if content, ok = qqCommandArg(trimmed, words...); !ok || content == "" {
		return content, false, ok
	}
	last, _ := utf8.DecodeLastRuneInString(strings.TrimSuffix(trimmed, content))
	return content, !strings.ContainsRune(qqCommandSeparators, last), true
}

func qqPolitePrefixCases() []struct {
	name, input string
	words       []string
} {
	body := strings.Repeat("候选框在哪里", 333)
	add := []string{"加词", "添加词条", "加个词"}
	docs := []string{"提文档", "文档反馈"}
	return []struct {
		name, input string
		words       []string
	}{
		{"普通短消息", "候选框在哪里？", add},
		{"普通两千字", body + "？", add},
		{"全角普通消息", "ＨＥＬＰ " + body + "？", docs},
		{"裸加词", "加词 扛把子 kang ba zi", add},
		{"客气加词", "帮我加个词 扛把子 kang ba zi", add},
		{"客气连写", "请加词扛把子 kang ba zi", add},
		{"裸长文档", "提文档 " + body, docs},
		{"客气长文档", "麻烦你提文档 " + body, docs},
		{"客气非命令", "帮我看看" + body, docs},
		{"重复客气", "请帮我加词 扛把子", add},
		{"Unicode空白", "\u3000请\u0085加词\u2002扛把子\u00a0", add},
		{"无效UTF8", "请加词 " + body + "\xff", add},
	}
}

func TestQQPolitePrefixAvoidsAllocation(t *testing.T) {
	for _, tc := range qqPolitePrefixCases() {
		t.Run(tc.name, func(t *testing.T) {
			wantContent, wantGlued, wantOK := referenceQQPoliteCommandArg(tc.input, tc.words...)
			var content string
			var glued, ok bool
			allocations := testing.AllocsPerRun(100, func() {
				content, glued, ok = qqPoliteCommandArg(tc.input, tc.words...)
			})
			if content != wantContent || glued != wantGlued || ok != wantOK {
				t.Fatalf("客气命令解析变化：结果=(%q,%t,%t)，期望=(%q,%t,%t)", content, glued, ok, wantContent, wantGlued, wantOK)
			}
			if allocations != 0 {
				t.Fatalf("去掉客气前缀分配 %.1f 次，期望零分配", allocations)
			}
		})
	}
}

func TestQQPolitePrefixMatchesPrevious(t *testing.T) {
	check := func(input string, words []string) {
		t.Helper()
		content, glued, ok := qqPoliteCommandArg(input, words...)
		wantContent, wantGlued, wantOK := referenceQQPoliteCommandArg(input, words...)
		if content != wantContent || glued != wantGlued || ok != wantOK {
			t.Fatalf("客气前缀解析不兼容：输入=%q，命令=%q，结果=(%q,%t,%t)，期望=(%q,%t,%t)", input, words, content, glued, ok, wantContent, wantGlued, wantOK)
		}
	}
	wordSets := [][]string{{"加词", "添加词条", "加个词"}, {"提文档", "文档反馈"}, {"记录答案", "记答案"}, nil}
	for _, prefix := range []string{"帮我", "帮忙", "请", "麻烦", "麻烦你", "我想", "我要", "想"} {
		for i := range len(prefix) {
			check(prefix[:i]+"加词 内容", wordSets[0])
		}
		for value := range 256 {
			check(prefix+string([]byte{byte(value)})+"加词 内容", wordSets[0])
		}
	}
	for _, prefix := range []string{"", "帮我", "帮忙", "请", "麻烦", "麻烦你", "我想", "我要", "想", "请帮我", "我想请", "麻烦你你"} {
		for _, space := range []string{"", " ", "\t\n\v\f\r", "\u0085", "\u00a0", "\u2002", "\u3000"} {
			for _, body := range []string{"", "加词", "加词 扛把子 kang ba zi", "加词扛把子 kang ba zi", "添加词条：内容", "提文档: 内容", "提文档的格式", "文档反馈\u3000内容", "记录答案 问：问题 答：回答", "普通问题", "加词 \xff", "提文档 \xe4\xb8"} {
				for _, words := range wordSets {
					check("\u3000"+prefix+space+body+"\t", words)
				}
			}
		}
	}
	units := []string{"", "帮我", "帮忙", "请", "麻烦", "你", "我想", "我要", "想", "加词", "提文档", "记录答案", "水", "杉", "A", "É", "Ｈ", "？", ":", "：", " ", "\t", "\n", "\u0085", "\u00a0", "\u3000", "\u200b", "🙏", "\ufffd", "\xff", "\xe4\xb8"}
	random := rand.New(rand.NewSource(958))
	for range 4000 {
		var input strings.Builder
		for range random.Intn(60) {
			input.WriteString(units[random.Intn(len(units))])
		}
		text := input.String()
		check(text, wordSets[random.Intn(len(wordSets))])
		check("帮我加词 "+text, wordSets[0])
		check("请提文档 "+text, wordSets[1])
	}
}

func TestQQPolitePrefixRemovesOneLayer(t *testing.T) {
	for _, tc := range []struct {
		input, content string
		glued, ok      bool
	}{
		{"帮我加词 扛把子", "扛把子", false, true},
		{"麻烦你加词扛把子", "扛把子", true, true},
		{"麻烦加词 扛把子", "扛把子", false, true},
		{"帮忙\t\n\f\r 加词 扛把子", "扛把子", false, true},
		{"请帮我加词 扛把子", "", false, false},
		{"我想请加词 扛把子", "", false, false},
	} {
		content, glued, ok := qqPoliteCommandArg(tc.input, "加词")
		if content != tc.content || glued != tc.glued || ok != tc.ok {
			t.Errorf("客气前缀层数或参数变化：输入=%q，结果=(%q,%t,%t)，期望=(%q,%t,%t)", tc.input, content, glued, ok, tc.content, tc.glued, tc.ok)
		}
	}
}

func BenchmarkQQPolitePrefix(b *testing.B) {
	for _, tc := range qqPolitePrefixCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _, _ = qqPoliteCommandArg(tc.input, tc.words...)
			}
		})
	}
}
