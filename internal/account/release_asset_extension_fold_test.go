package account

import (
	"strings"
	"testing"
)

func previousReleaseAssetExtension(asset string) string {
	if dot := strings.LastIndexByte(asset, '.'); dot >= 0 {
		return strings.ToLower(asset[dot:])
	}
	return ""
}

func TestReleaseAssetExtensionFoldCompatibility(t *testing.T) {
	for _, asset := range []string{"setup.exe", "setup.AppImage", "setup.APPIMAGE", "source.tar.gz", "setup.İPA", "setup.\xff", "readme"} {
		if got, want := releaseAssetExtension(asset), previousReleaseAssetExtension(asset); got != want {
			t.Errorf("资产=%q：结果 %q，原结果 %q", asset, got, want)
		}
	}
}

func TestReleaseAssetExtensionFoldUppercaseAllocations(t *testing.T) {
	for _, asset := range []string{"setup.APPIMAGE", "setup.EXE", "setup.YAML"} {
		if allocations := testing.AllocsPerRun(1000, func() { _ = releaseAssetExtension(asset) }); allocations != 0 {
			t.Errorf("资产=%q 分配 %.0f 次，期望零分配", asset, allocations)
		}
	}
}
