package server

import (
	"strings"
	"testing"
)

func previousQQWindowsProductTextHasWord(text string, words []string) bool {
	lower := strings.ToLower(text)
	return slicesContainsStringFunc(words, func(word string) bool { return strings.Contains(lower, word) })
}

func slicesContainsStringFunc(words []string, match func(string) bool) bool {
	for _, word := range words {
		if match(word) {
			return true
		}
	}
	return false
}

func TestQQWindowsProductTextHasWordCompatibility(t *testing.T) {
	for _, tc := range []struct {
		text  string
		words []string
	}{
		{"MetasequoiaIME-PINYIN_Setup", qqWindowsNewWords},
		{"水杉拼音 MetasequoiaIME_Setup", qqWindowsNewWords},
		{"MetasequoiaIME_Setup", qqWindowsOldWords},
		{"İOS 安装包", []string{"ios"}},
		{"没有匹配词", qqWindowsNewWords},
	} {
		got := qqWindowsProductTextHasWord(tc.text, tc.words)
		want := previousQQWindowsProductTextHasWord(tc.text, tc.words)
		if got != want {
			t.Errorf("文本=%q：结果 %t，原结果 %t", tc.text, got, want)
		}
	}
}

func TestQQWindowsProductTextHasWordAllocations(t *testing.T) {
	if allocations := testing.AllocsPerRun(1000, func() { _ = qqWindowsProductTextHasWord("MetasequoiaIME-PINYIN_Setup", qqWindowsNewWords) }); allocations != 0 {
		t.Errorf("ASCII 文本分配 %.0f 次，期望零分配", allocations)
	}
}

var qqWindowsProductTextBenchmarkResult bool

func BenchmarkQQWindowsProductTextHasWord(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		words      []string
	}{
		{"ASCII匹配", "MetasequoiaIME-PINYIN_Setup", qqWindowsNewWords},
		{"ASCII无匹配", "候选框位置不对", qqWindowsNewWords},
		{"Unicode匹配", "水杉拼音安装包", qqWindowsNewWords},
	} {
		b.Run(tc.name+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqWindowsProductTextBenchmarkResult = previousQQWindowsProductTextHasWord(tc.text, tc.words)
			}
		})
		b.Run(tc.name+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqWindowsProductTextBenchmarkResult = qqWindowsProductTextHasWord(tc.text, tc.words)
			}
		})
	}
}
