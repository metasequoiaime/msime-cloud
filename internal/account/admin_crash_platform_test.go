package account

import (
	"strings"
	"testing"
)

func previousCrashPlatformName(platform string) string {
	if name, ok := crashPlatformNames[strings.ToLower(platform)]; ok {
		return name
	}
	return platform
}

func previousCrashPlatform(platform string) string {
	switch platform = strings.ToLower(platform); platform {
	case "win":
		return "windows"
	case "mac", "darwin":
		return "macos"
	case "ipados":
		return "ios"
	case "harmony", "ohos":
		return "harmonyos"
	}
	return platform
}

func TestCrashPlatformFastPathsMatchPrevious(t *testing.T) {
	for _, platform := range []string{
		"", "ios", "iOS", "IOS", "steamdeck", "SteamDeck", "鸿蒙OS", "İOS", "\xffOS", "win", "WIN", "darwin", "OHOS", "windows ", "macOS!",
	} {
		if got, want := crashPlatformName(platform), previousCrashPlatformName(platform); got != want {
			t.Errorf("平台名称 %q：结果 %q，原结果 %q", platform, got, want)
		}
		if got, want := crashPlatform(platform), previousCrashPlatform(platform); got != want {
			t.Errorf("平台键 %q：结果 %q，原结果 %q", platform, got, want)
		}
	}
}

func TestCrashPlatformLowerFastPathsAllocateZero(t *testing.T) {
	for _, platform := range []string{"ios", "steamdeck", "macos", "harmonyos"} {
		if allocations := testing.AllocsPerRun(1000, func() { _ = crashPlatformName(platform) }); allocations != 0 {
			t.Errorf("平台名称 %q 分配 %.0f 次，期望零分配", platform, allocations)
		}
		if allocations := testing.AllocsPerRun(1000, func() { _ = crashPlatform(platform) }); allocations != 0 {
			t.Errorf("平台键 %q 分配 %.0f 次，期望零分配", platform, allocations)
		}
	}
}

var crashPlatformBenchmarkResult string

func BenchmarkCrashPlatformFastPaths(b *testing.B) {
	for _, tc := range []struct {
		name, platform string
	}{
		{"小写已知", "ios"},
		{"小写未知", "steamdeck"},
		{"大写已知", "iOS"},
		{"Unicode", "鸿蒙OS"},
	} {
		for _, impl := range []struct {
			name string
			fn   func(string) string
		}{
			{"原实现名称", previousCrashPlatformName},
			{"当前实现名称", crashPlatformName},
			{"原实现键", previousCrashPlatform},
			{"当前实现键", crashPlatform},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					crashPlatformBenchmarkResult = impl.fn(tc.platform)
				}
			})
		}
	}
}
