package server

import (
	"context"
	"testing"
)

func TestQQWindowsProductSharesUnicodeLowercase(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		text string
		max  float64
	}{
		{"Windows 候选框位置不对", 1},
		{"安装 MetasequoiaIME_Setup", 1},
		{"安装 MetasequoiaIME-PINYIN_Setup", 1},
		{"MetasequoiaIME-PINYIN_Setup", 0},
	} {
		allocs := testing.AllocsPerRun(100, func() {
			qqWindowsProductTextBenchmarkResult, _ = s.qqWindowsProduct(context.Background(), [3]int{}, tc.text)
		})
		if allocs > tc.max {
			t.Errorf("文本=%q：分配 %.0f 次，期望最多 %.0f 次", tc.text, allocs, tc.max)
		}
	}
}

func TestQQWindowsProductSharedLowercaseCompatibility(t *testing.T) {
	s := &Server{}
	for _, text := range []string{
		"Windows 候选框位置不对", "安装 MetasequoiaIME_Setup", "安装 MetasequoiaIME-PINYIN_Setup",
		"水杉拼音 MetasequoiaIME_Setup", "MetasequoiaIME-PİNYİN_Setup 安装", "\xffMetasequoiaIME_Setup 安装",
		"MetasequoiaIME-PINYIN_Setup", "", "普通反馈",
	} {
		got, question := s.qqWindowsProduct(context.Background(), [3]int{}, text)
		if want := previousQQWindowsProductUnicodeClassification(text); got != want || question != "" {
			t.Errorf("文本=%q：新版=%v，追问=%q，期望新版=%v 且无追问", text, got, question, want)
		}
	}
}

func previousQQWindowsProductUnicodeClassification(text string) bool {
	if previousQQWindowsProductTextHasWord(text, qqWindowsNewWords) {
		return true
	}
	if previousQQWindowsProductTextHasWord(text, qqWindowsOldWords) {
		return false
	}
	return false
}

func BenchmarkQQWindowsProductUnicodeClassification(b *testing.B) {
	s := &Server{}
	for _, tc := range []struct{ name, text string }{
		{"无关键词", "Windows 候选框位置不对"},
		{"旧版安装包", "安装 MetasequoiaIME_Setup"},
		{"新版安装包", "安装 MetasequoiaIME-PINYIN_Setup"},
	} {
		b.Run(tc.name+"/修改前", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqWindowsProductTextBenchmarkResult = previousQQWindowsProductUnicodeClassification(tc.text)
			}
		})
		b.Run(tc.name+"/修改后", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqWindowsProductTextBenchmarkResult, _ = s.qqWindowsProduct(context.Background(), [3]int{}, tc.text)
			}
		})
	}
}
