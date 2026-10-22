package server

import (
	"strings"
	"testing"
)

func TestQQContainsLowerPlatformWord(t *testing.T) {
	for _, tc := range []struct {
		question, word string
		want           bool
	}{
		{"windows 上候选框", "Windows", true}, {"harmony 版", "HarmonyOS", false}, {"harmonyos 版", "HarmonyOS", true},
		{"苹果电脑", "macOS", false}, {"苹果电脑", "苹果", true}, {"", "ios", false}, {"x", "", false},
	} {
		if got := qqContainsLowerPlatformWord(tc.question, tc.word); got != tc.want {
			t.Errorf("问题=%q 词=%q：结果 %t，期望 %t", tc.question, tc.word, got, tc.want)
		}
	}
}

func previousQQMentionsPlatformLower(question string, p qqPlatformRelease) bool {
	if p.Name != "" && strings.Contains(question, strings.ToLower(p.Name)) {
		return true
	}
	if p.ID != "" && strings.Contains(question, strings.ToLower(p.ID)) {
		return true
	}
	for _, word := range qqPlatformWords[p.ID] {
		if strings.Contains(question, word) {
			return true
		}
	}
	return false
}

func TestQQPlatformMentionFoldCompatibility(t *testing.T) {
	for _, tc := range []struct {
		question string
		platform qqPlatformRelease
	}{
		{"windows 上候选框", qqPlatformRelease{Name: "Windows", ID: "windows"}}, {"harmonyos 版", qqPlatformRelease{Name: "HarmonyOS", ID: "harmony"}},
		{"苹果电脑安装包", qqPlatformRelease{Name: "macOS", ID: "macos"}}, {"问题", qqPlatformRelease{Name: "问题", ID: "custom"}},
	} {
		if got, want := qqMentionsPlatformLower(tc.question, tc.platform), previousQQMentionsPlatformLower(tc.question, tc.platform); got != want {
			t.Errorf("%q/%+v：结果 %t，原结果 %t", tc.question, tc.platform, got, want)
		}
	}
}

var qqPlatformMentionFoldBenchmarkResult bool

func BenchmarkQQPlatformMentionFold(b *testing.B) {
	p := qqPlatformRelease{Name: "Windows", ID: "windows"}
	for _, question := range []string{"windows 上候选框位置不对", "linux 上候选框位置不对"} {
		b.Run(question+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqPlatformMentionFoldBenchmarkResult = previousQQMentionsPlatformLower(question, p)
			}
		})
		b.Run(question+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqPlatformMentionFoldBenchmarkResult = qqMentionsPlatformLower(question, p)
			}
		})
	}
}
