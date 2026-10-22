package account

import (
	"strings"
	"testing"
)

func previousLegacySessionPlatform(platform string) string {
	platform = strings.ToLower(platform)
	if sessionPlatforms[platform] {
		return platform
	}
	return ""
}

func TestLegacySessionPlatformCompatibility(t *testing.T) {
	for _, platform := range []string{"Android", "IOS", "MacOS", "WINDOWS", "Linux", "Harmony", "unknown", "İOS", "\xff"} {
		if got, want := legacySessionPlatform(platform), previousLegacySessionPlatform(platform); got != want {
			t.Errorf("平台=%q：结果 %q，原结果 %q", platform, got, want)
		}
	}
}

func TestLegacySessionPlatformAllocations(t *testing.T) {
	for _, platform := range []string{"Android", "WINDOWS", "Harmony"} {
		if allocations := testing.AllocsPerRun(1000, func() { _ = legacySessionPlatform(platform) }); allocations != 0 {
			t.Errorf("平台=%q 分配 %.0f 次，期望零分配", platform, allocations)
		}
	}
}

var legacySessionPlatformBenchmarkResult string

func BenchmarkLegacySessionPlatformFold(b *testing.B) {
	for _, platform := range []string{"Android", "WINDOWS", "unknown"} {
		b.Run(platform+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				legacySessionPlatformBenchmarkResult = previousLegacySessionPlatform(platform)
			}
		})
		b.Run(platform+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				legacySessionPlatformBenchmarkResult = legacySessionPlatform(platform)
			}
		})
	}
}
