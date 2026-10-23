package server

import (
	"strings"
	"testing"
)

func benchmarkReleasePlatformList() []qqPlatformRelease {
	list := make([]qqPlatformRelease, 6)
	for i := range list {
		list[i] = qqPlatformRelease{
			ID:   []string{"windows", "macos", "android", "ios", "harmony", "linux"}[i],
			Name: []string{"Windows", "macOS", "Android", "iOS", "HarmonyOS", "Linux"}[i],
			Repo: "o/repo",
			Stable: &qqRelease{
				TagName: "v0.10.1", PublishedAt: "2026-10-11T00:00:00Z",
				notes: "合成更新内容", notesReady: true,
			},
		}
	}
	return list
}

func previousQQReleasePlatformMaterial(question string, list []qqPlatformRelease) string {
	return qqReleasesMaterial(list, func(p qqPlatformRelease) bool { return qqMentionsPlatform(question, p) })
}

func currentQQReleasePlatformMaterial(question string, list []qqPlatformRelease) string {
	lower := strings.ToLower(question)
	return qqReleasesMaterial(list, func(p qqPlatformRelease) bool { return qqMentionsPlatformLower(lower, p) })
}

func BenchmarkQQReleasePlatformQuery(b *testing.B) {
	list := benchmarkReleasePlatformList()
	question := "Windows 和 Android 的新版更新了什么"
	for _, impl := range []struct {
		name string
		run  func(string, []qqPlatformRelease) string
	}{
		{"previous", previousQQReleasePlatformMaterial},
		{"current", currentQQReleasePlatformMaterial},
	} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if impl.run(question, list) == "" {
					b.Fatal("版本资料为空")
				}
			}
		})
	}
}
