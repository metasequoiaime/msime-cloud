package account

import (
	"strings"
	"testing"
)

func previousCanonicalPlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if canonical, ok := platformAliases[platform]; ok {
		return canonical
	}
	return platform
}

func TestCanonicalPlatformCompatibility(t *testing.T) {
	for _, platform := range []string{"windows", "Windows", "WIN", "macOS", "DARWIN", "harmonyOS", "OHOS", " unknown ", "İOS", "鸿蒙", ""} {
		if got, want := canonicalPlatform(platform), previousCanonicalPlatform(platform); got != want {
			t.Errorf("平台=%q：结果 %q，原结果 %q", platform, got, want)
		}
	}
}

func TestCanonicalPlatformAllocations(t *testing.T) {
	for _, platform := range []string{"windows", "Windows", "WIN", "harmonyOS"} {
		if allocations := testing.AllocsPerRun(1000, func() { _ = canonicalPlatform(platform) }); allocations != 0 {
			t.Errorf("平台=%q 分配 %.0f 次，期望零分配", platform, allocations)
		}
	}
}

var canonicalPlatformBenchmarkResult string

func BenchmarkCanonicalPlatform(b *testing.B) {
	for _, platform := range []string{"windows", "Windows", "WIN", "unknown", "İOS"} {
		b.Run(platform+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				canonicalPlatformBenchmarkResult = previousCanonicalPlatform(platform)
			}
		})
		b.Run(platform+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				canonicalPlatformBenchmarkResult = canonicalPlatform(platform)
			}
		})
	}
}
