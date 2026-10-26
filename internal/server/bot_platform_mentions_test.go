package server

import (
	"strings"
	"testing"
)

// previousQQMentionsPlatform 保留优化前实现，用于兼容性和性能比较。
func previousQQMentionsPlatform(question string, p qqPlatformRelease) bool {
	question = strings.ToLower(question)
	for _, word := range append([]string{p.Name, p.ID}, qqPlatformWords[p.ID]...) {
		if word != "" && strings.Contains(question, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

func TestQQMentionsPlatformCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, question string
		platform       qqPlatformRelease
		want           bool
	}{
		{"name", "Windows 上候选框", qqPlatformRelease{Name: "Windows", ID: "windows"}, true},
		{"id", "HARMONY 版", qqPlatformRelease{Name: "HarmonyOS", ID: "harmony"}, true},
		{"alias", "苹果电脑安装包", qqPlatformRelease{Name: "macOS", ID: "macos"}, true},
		{"missing", "Linux 上候选框", qqPlatformRelease{Name: "Windows", ID: "windows"}, false},
		{"empty", "Windows", qqPlatformRelease{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qqMentionsPlatform(tc.question, tc.platform); got != tc.want {
				t.Fatalf("平台提及判断=%v，期望=%v", got, tc.want)
			}
		})
	}
}

func BenchmarkQQMentionsPlatform(b *testing.B) {
	platform := qqPlatformRelease{Name: "Windows", ID: "windows"}
	for _, tc := range []struct {
		name, question string
	}{
		{"match", "Windows 上候选框位置不对"},
		{"alias", "微软输入法设置"},
		{"missing", "Linux 上候选框位置不对"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for _, impl := range []struct {
				name string
				run  func(string, qqPlatformRelease) bool
			}{{"previous", previousQQMentionsPlatform}, {"current", qqMentionsPlatform}} {
				b.Run(impl.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.run(tc.question, platform)
					}
				})
			}
		})
	}
}
