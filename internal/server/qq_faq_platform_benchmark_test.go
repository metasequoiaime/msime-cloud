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
	for _, title := range []string{"安装与启动 > Windows 版为什么启动失败", "macOS 上候选框", "HarmonyOS 安装", "鸿蒙输入法", "没有平台词", "İOS 安装", "\xffWindows 版"} {
		if got, want := qqFAQPlatform(title), previousQQFAQPlatform(title); got != want {
			t.Errorf("标题=%q：结果 %q，原结果 %q", title, got, want)
		}
	}
}

func TestQQFAQPlatformPreservesUnicodeWordBytes(t *testing.T) {
	for _, title := range []string{"ABC 鸿草输入问题", "ABC 鸡菜输入问题", "ABC 鸿蒙输入问题"} {
		if got, want := qqFAQPlatform(title), previousQQFAQPlatform(title); got != want {
			t.Errorf("标题=%q：平台=%q，期望=%q", title, got, want)
		}
	}
}

func TestQQFAQPlatformUnicodePriority(t *testing.T) {
	for _, title := range []string{
		"İOS 鸿蒙", "LİNUX macOS", "WİNDOWS iOS", "İPAD HarmonyOS",
		"鸿蒙 İOS", "macOS LİNUX", "iOS WİNDOWS", "HarmonyOS İPAD",
		"\xffİOS 鸿蒙", "WİNDOWS\xffiOS", "支持哪些系统，去哪里下载 İOS 鸿蒙",
	} {
		if got, want := qqFAQPlatform(title), previousQQFAQPlatform(title); got != want {
			t.Errorf("标题=%q：结果 %q，原结果 %q", title, got, want)
		}
	}
	for _, left := range []string{"WİNDOWS", "macOS", "LİNUX", "İOS", "İPAD", "鸿蒙", "HarmonyOS"} {
		for _, right := range []string{"Windows", "macOS", "Linux", "iOS", "iPad", "鸿蒙", "HarmonyOS"} {
			for _, title := range []string{left + " / " + right, right + " / " + left} {
				if got, want := qqFAQPlatform(title), previousQQFAQPlatform(title); got != want {
					t.Errorf("标题=%q：结果 %q，原结果 %q", title, got, want)
				}
			}
		}
	}
}

func TestQQFAQPlatformCommonTitlesNoAllocation(t *testing.T) {
	for _, title := range []string{"Windows 版为什么启动失败", "macOS 上候选框", "鸿蒙输入法", "这个问题没有平台词但标题很长"} {
		allocs := testing.AllocsPerRun(100, func() { qqFAQPlatformBenchmarkResult = qqFAQPlatform(title) })
		if allocs != 0 {
			t.Errorf("标题=%q：分配 %.0f 次，期望零分配", title, allocs)
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
