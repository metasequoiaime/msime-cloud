package server

import (
	"strings"
	"testing"
)

var previousQQFAQTitlePlatforms = [][2]string{{"Windows", "Windows"}, {"macOS", "macOS"}, {"Linux", "Linux"}, {"iOS", "iOS"}, {"iPad", "iOS"}, {"鸿蒙", "鸿蒙"}, {"HarmonyOS", "鸿蒙"}}

func previousQQFAQPlatform(title string) string {
	for _, phrase := range qqFAQAllPlatforms {
		if strings.Contains(title, phrase) {
			return "各平台"
		}
	}
	lower := strings.ToLower(title)
	for _, pair := range previousQQFAQTitlePlatforms {
		if strings.Contains(lower, strings.ToLower(pair[0])) {
			return pair[1]
		}
	}
	return qqFAQDefaultPlatform
}

func TestQQFAQPlatformLowercaseCache(t *testing.T) {
	for _, title := range []string{"安装与启动 > Windows 版为什么启动失败", "macOS 上候选框", "HarmonyOS 安装", "鸿蒙输入法", "没有平台词"} {
		if got, want := qqFAQPlatform(title), previousQQFAQPlatform(title); got != want {
			t.Errorf("标题=%q：结果 %q，原结果 %q", title, got, want)
		}
	}
}

var qqFAQPlatformBenchmarkResult string

func BenchmarkQQFAQPlatformLowercaseCache(b *testing.B) {
	for _, tc := range []struct{ name, title string }{
		{"Windows", "安装与启动 > Windows 版为什么启动失败"},
		{"无平台", "这个问题没有平台词但标题很长"},
		{"Unicode", "安装与启动 > 鸿蒙版为什么启动失败"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string) string
		}{{"原实现", previousQQFAQPlatform}, {"当前实现", qqFAQPlatform}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqFAQPlatformBenchmarkResult = impl.fn(tc.title)
				}
			})
		}
	}
}
