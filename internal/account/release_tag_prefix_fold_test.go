package account

import "testing"

func TestReleaseTagPlatformPrefixCompatibility(t *testing.T) {
	for _, prefix := range []string{"windows", "Windows", "WINDOWS", "msime", "LINUX", "İOS", "win\xff"} {
		got, ok := releaseTagPlatformPrefix(prefix)
		want, wantOK := releaseTagPlatforms[prefix], false
		if want != "" {
			wantOK = true
		}
		if !wantOK {
			for known, platform := range releaseTagPlatforms {
				if len(prefix) == len(known) && equalFoldReleaseTag(prefix, known) {
					want, wantOK = platform, true
				}
			}
		}
		if got != want || ok != wantOK {
			t.Errorf("前缀=%q：结果=(%q,%t)，期望=(%q,%t)", prefix, got, ok, want, wantOK)
		}
	}
}

func equalFoldReleaseTag(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ac, bc := a[i], b[i]
		if ac >= 'A' && ac <= 'Z' {
			ac += 'a' - 'A'
		}
		if bc >= 'A' && bc <= 'Z' {
			bc += 'a' - 'A'
		}
		if ac != bc {
			return false
		}
	}
	return true
}

var releaseTagPrefixBenchmarkResult string

func BenchmarkReleaseTagPlatformPrefix(b *testing.B) {
	for _, prefix := range []string{"windows", "Windows", "unknown"} {
		b.Run(prefix, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if p, ok := releaseTagPlatformPrefix(prefix); ok {
					releaseTagPrefixBenchmarkResult = p
				}
			}
		})
	}
}
