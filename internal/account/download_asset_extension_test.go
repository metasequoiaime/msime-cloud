package account

import (
	"math/rand"
	"strings"
	"testing"
)

func previousReleaseTagPlatform(tag, asset string) (string, string) {
	if m := releaseTag.FindStringSubmatch(tag); m != nil {
		if platform, ok := releaseTagPlatforms[strings.ToLower(m[1])]; ok {
			return platform, m[2]
		}
	}
	name := strings.ToLower(asset)
	for _, rule := range []struct{ platform, suffixes string }{
		{"windows", ".exe .msi .msix .appx"},
		{"macos", ".dmg .pkg"},
		{"linux", ".deb .rpm .appimage .flatpak .snap"},
		{"android", ".apk .aab"},
		{"ios", ".ipa"},
		{"harmony", ".hap .app"},
	} {
		for _, suffix := range strings.Fields(rule.suffixes) {
			if strings.HasSuffix(name, suffix) {
				return rule.platform, tag
			}
		}
	}
	return "", tag
}

func TestReleaseTagPlatformExtensionCompatibility(t *testing.T) {
	assets := []string{"", "setup", ".", "setup.exe ", "setup.exe.sig", "setup.exe/child", "bundle.app/child", "a.b.c", "安装包😀", "\xff.exe", "bundle.İPA", "bundle.KPK"}
	for _, suffix := range strings.Fields(".exe .msi .msix .appx .dmg .pkg .deb .rpm .appimage .flatpak .snap .apk .aab .ipa .hap .app") {
		for _, prefix := range []string{"", "setup", "a.b/安装包😀", "\xff"} {
			assets = append(assets, prefix+suffix, prefix+strings.ToUpper(suffix))
		}
	}
	rng := rand.New(rand.NewSource(91))
	for size := 0; size < 512; size++ {
		data := make([]byte, size)
		if _, err := rng.Read(data); err != nil {
			t.Fatal(err)
		}
		assets = append(assets, string(data), string(data)+".APPIMAGE")
	}
	for _, tag := range []string{"nightly", "v1.2.3", "msime-v1.2.3", "Windows-v1.2.3", "LINUX-1.0", "HarmonyOS-v1.0.0"} {
		for _, asset := range assets {
			gotPlatform, gotVersion := releaseTagPlatform(tag, asset)
			wantPlatform, wantVersion := previousReleaseTagPlatform(tag, asset)
			if gotPlatform != wantPlatform || gotVersion != wantVersion {
				t.Fatalf("安装包分类变化：tag=%q，asset=%q，结果=(%q,%q)，期望=(%q,%q)", tag, asset, gotPlatform, gotVersion, wantPlatform, wantVersion)
			}
		}
	}
}

func TestReleaseTagPlatformFallbackAllocationBudget(t *testing.T) {
	for _, asset := range []string{"setup.exe", "setup.dmg", "setup.appimage", "setup.apk", "setup.ipa", "setup.hap", "source.tar.gz", "readme"} {
		t.Run(asset, func(t *testing.T) {
			var platform, version string
			allocations := testing.AllocsPerRun(50, func() {
				platform, version = releaseTagPlatform("nightly", asset)
			})
			if wantPlatform, wantVersion := previousReleaseTagPlatform("nightly", asset); platform != wantPlatform || version != wantVersion {
				t.Fatal("安装包分类结果变化")
			}
			if allocations != 0 {
				t.Fatalf("小写文件名的扩展名分类分配 %.0f 次，期望无需构造规则切片", allocations)
			}
		})
	}
}

func BenchmarkReleaseTagPlatformExtension(b *testing.B) {
	for _, tc := range []struct{ name, tag, asset string }{
		{"tag", "windows-v1.2.3", "setup.exe"},
		{"windows", "v1.2.3", "setup.exe"},
		{"linux", "v1.2.3", "setup.appimage"},
		{"harmony", "v1.2.3", "setup.hap"},
		{"unknown", "nightly", "source.tar.gz"},
		{"uppercase", "v1.2.3", "MSIME-1.2.3.AppImage"},
		{"product_tag", "msime-v1.2.3", "setup.apk"},
	} {
		for _, impl := range []struct {
			name     string
			classify func(string, string) (string, string)
		}{{"previous", previousReleaseTagPlatform}, {"current", releaseTagPlatform}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var platform, version string
				for b.Loop() {
					platform, version = impl.classify(tc.tag, tc.asset)
				}
				if wantPlatform, wantVersion := previousReleaseTagPlatform(tc.tag, tc.asset); platform != wantPlatform || version != wantVersion {
					b.Fatal("安装包分类结果变化")
				}
			})
		}
	}
}
