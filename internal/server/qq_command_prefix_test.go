package server

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func previousQQCommandArg(text string, words ...string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	for _, word := range words {
		if !strings.HasPrefix(lower, word) {
			continue
		}
		rest := trimmed[len(word):]
		if rest == "" {
			return "", true
		}
		last := word[len(word)-1]
		loose := slices.Contains(qqLooseCommands, word) || last >= 'a' && last <= 'z'
		if first, _ := utf8.DecodeRuneInString(rest); loose || strings.ContainsRune(qqCommandSeparators, first) {
			return strings.TrimSpace(strings.TrimLeft(rest, qqCommandSeparators)), true
		}
	}
	return "", false
}

func TestQQCommandArgMatchesPreviousPrefix(t *testing.T) {
	commands := [][]string{
		nil,
		{"查", "搜索", "搜"},
		{"加词", "添加词条", "加个词"},
		{"提文档", "文档反馈"},
		{"提issue", "提 issue", "提交issue"},
		{"记录答案", "记答案"},
		{"i", "k", "s", "σ", "ȿ", "ⱥ", "�"},
		{"提is", "提issue"},
		{"ISSUE", "Issue"},
		{"iiiiiiii", "kkkkkkkk"},
	}
	texts := []string{"", " \t\u3000", "查", "查不到词库怎么办", "提ISSUE", "提İSSUE 内容", "K:ABC", "ſ：ABC", "Σ：ABC", "Ȿ：ABC", "Ⱥ：ABC", "\xff：ABC", "提\xffISSUE：ABC"}
	texts = append(texts, strings.Repeat("İ", 8)+"：ABC", strings.Repeat("K", 8)+"：ABC")
	for _, words := range commands {
		for _, word := range words {
			for _, separator := range []string{"", ":", "：", " ", "\n", "\t", ",", "，", "、", "\r", "\v", "\u0085", "\u3000", "😀"} {
				texts = append(texts, "\u3000"+word+separator+"ABC 候选框\t ", strings.ToUpper(word)+separator+"ABC")
			}
		}
	}
	random := rand.New(rand.NewSource(31))
	units := []string{"提", "查", "加词", "文档", "issue", "ISSUE", " ", "\t", "\n", "\r", "：", "，", "\u3000", "İ", "K", "ſ", "Σ", "Ȿ", "Ⱥ", "\xff", "\xc0", "\xe4\xb8", "😀"}
	for range 1000 {
		var text strings.Builder
		for range random.Intn(30) {
			text.WriteString(units[random.Intn(len(units))])
		}
		texts = append(texts, text.String()+":abcdefghijklmnop")
	}
	for _, text := range texts {
		for _, words := range commands {
			got, ok := qqCommandArg(text, words...)
			want, wantOK := previousQQCommandArg(text, words...)
			if got != want || ok != wantOK {
				t.Fatalf("命令解析不兼容：输入=%q，命令=%q，结果=(%q, %v)，期望=(%q, %v)", text, words, got, ok, want, wantOK)
			}
		}
	}
}

func TestQQCommandArgAvoidsTailAllocation(t *testing.T) {
	tail := strings.Repeat("ABC 候选框 ", 1<<14)
	for _, tc := range []struct {
		name, text string
		words      []string
	}{
		{"匹配命令", "提ISSUE：" + tail, []string{"提issue", "提 issue", "提交issue"}},
		{"普通消息", "普通问题 " + tail, []string{"查", "搜索", "搜"}},
		{"没有命令", tail, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantOK := previousQQCommandArg(tc.text, tc.words...)
			var got string
			var ok bool
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got, ok = qqCommandArg(tc.text, tc.words...)
				}
			})
			if got != want || ok != wantOK {
				t.Fatal("长消息的命令解析结果发生变化")
			}
			if allocated := result.AllocedBytesPerOp(); allocated > 256 {
				t.Fatalf("命令前缀判断分配 %d 字节，超过 256 字节预算", allocated)
			}
		})
	}
}

func BenchmarkQQCommandArgPrefix(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		words      []string
	}{
		{"short_match", "提ISSUE：Windows 候选框位置错误", []string{"提issue", "提 issue", "提交issue"}},
		{"short_plain", "Windows 候选框位置错误", []string{"查", "搜索", "搜"}},
		{"short_chinese_plain", "候选框在哪里？", []string{"查", "搜索", "搜"}},
		{"chinese_message_plain", strings.Repeat("候选框在哪里", 333), []string{"查", "搜索", "搜"}},
		{"unicode_fallback", "Σ：ABC", []string{"σ", "ȿ", "ⱥ", "�"}},
		{"invalid_utf8_fallback", "\xff：ABC", []string{"�"}},
		{"message_match", "提ISSUE：" + strings.Repeat("Windows 候选框位置错误 ", 100), []string{"提issue", "提 issue", "提交issue"}},
		{"message_plain", strings.Repeat("Windows 候选框位置错误 ", 100), []string{"查", "搜索", "搜"}},
		{"long_tail", "提ISSUE：" + strings.Repeat("ABC 候选框 ", 1<<14), []string{"提issue", "提 issue", "提交issue"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = qqCommandArg(tc.text, tc.words...)
			}
		})
	}
}

func BenchmarkQQCommandArgTrimReuse(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		words      []string
	}{
		{"short", "帮我加词 扛把子", []string{"加词", "添加词条", "加个词"}},
		{"long", "帮我加词 " + strings.Repeat("候选框在哪里", 333), []string{"加词", "添加词条", "加个词"}},
		{"plain", strings.Repeat("候选框在哪里", 333), []string{"加词", "添加词条", "加个词"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			trimmed := strings.TrimSpace(tc.text)
			prefix := qqCommandPolitePrefixes[0]
			if rest, found := strings.CutPrefix(trimmed, prefix); found {
				trimmed = strings.TrimLeft(rest, "\t\n\f\r ")
			}
			b.ReportAllocs()
			for b.Loop() {
				_, _ = qqCommandArgTrimmed(trimmed, tc.words...)
			}
		})
		b.Run(tc.name+"_previous", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = qqCommandArg(strings.TrimSpace(tc.text), tc.words...)
			}
		})
	}
}
