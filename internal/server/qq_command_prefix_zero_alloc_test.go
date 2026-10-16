package server

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func previousQQCommandArgZeroAlloc(text string, words ...string) (string, bool) {
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

func TestQQCommandArgPrefixZeroAllocMatchesPrevious(t *testing.T) {
	words := [][]string{
		{"查", "搜索", "搜"},
		{"提issue", "提 issue", "提交issue"},
		{"加词", "添加词条", "加个词"},
		{"σ", "ȿ", "ⱥ", "�"},
		{"ISSUE"},
	}
	texts := []string{
		"", "普通问题", "查不到词库怎么办", "查：水杉", "提ISSUE：Windows", "提issue候选框",
		"提İSSUE 内容", "Σ：ABC", "Ȿ：ABC", "Ⱥ：ABC", "\xff：ABC", "提\xffISSUE：ABC",
		strings.Repeat("İ", 8) + "：ABC", strings.Repeat("K", 8) + "：ABC",
	}
	for _, text := range texts {
		for _, candidates := range words {
			got, gotOK := qqCommandArg(text, candidates...)
			want, wantOK := previousQQCommandArgZeroAlloc(text, candidates...)
			if got != want || gotOK != wantOK {
				t.Fatalf("命令解析不兼容：输入=%q，命令=%q，结果=(%q, %v)，期望=(%q, %v)", text, candidates, got, gotOK, want, wantOK)
			}
		}
	}
}

func TestQQCommandArgPrefixZeroAllocation(t *testing.T) {
	tail := strings.Repeat("ABC 候选框 ", 1<<14)
	for _, tc := range []struct {
		name, text string
		words      []string
	}{
		{"匹配小写命令", "提issue：" + tail, []string{"提issue", "提 issue", "提交issue"}},
		{"匹配大写命令", "提ISSUE：" + tail, []string{"提issue", "提 issue", "提交issue"}},
		{"普通长消息", "普通问题 " + tail, []string{"查", "搜索", "搜"}},
		{"没有命令", tail, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantOK := previousQQCommandArgZeroAlloc(tc.text, tc.words...)
			var got string
			var gotOK bool
			allocations := testing.AllocsPerRun(100, func() {
				got, gotOK = qqCommandArg(tc.text, tc.words...)
			})
			if got != want || gotOK != wantOK {
				t.Fatalf("长消息的命令解析结果发生变化：(%q, %v)，期望=(%q, %v)", got, gotOK, want, wantOK)
			}
			if allocations != 0 {
				t.Fatalf("命令前缀判断分配 %.1f 次，期望零分配", allocations)
			}
		})
	}
}
