package server

import (
	"strings"
	"testing"
)

func previousQQReportPlatform(platform string) string {
	platform = strings.ToLower(platform)
	if _, known := qqPlatformNames[platform]; platform != "" && !known {
		return ""
	}
	return platform
}

func TestQQReportPlatformCompatibility(t *testing.T) {
	for _, platform := range []string{"windows", "Windows", "WINDOWS", "macOS", "HarmonyOS", "unknown", "鸿蒙", "", "İOS"} {
		if got, want := qqReportPlatform(platform), previousQQReportPlatform(platform); got != want {
			t.Errorf("平台=%q：结果 %q，原结果 %q", platform, got, want)
		}
	}
}

func TestQQReportPlatformAllocations(t *testing.T) {
	for _, platform := range []string{"windows", "Windows", "HarmonyOS", "unknown"} {
		if allocations := testing.AllocsPerRun(1000, func() { _ = qqReportPlatform(platform) }); allocations != 0 {
			t.Errorf("平台=%q 分配 %.0f 次，期望零分配", platform, allocations)
		}
	}
}

var qqReportPlatformBenchmarkResult string

func BenchmarkQQReportPlatformFold(b *testing.B) {
	for _, platform := range []string{"windows", "Windows", "unknown", "鸿蒙"} {
		b.Run(platform+"/原实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqReportPlatformBenchmarkResult = previousQQReportPlatform(platform)
			}
		})
		b.Run(platform+"/当前实现", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				qqReportPlatformBenchmarkResult = qqReportPlatform(platform)
			}
		})
	}
}
