package account

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

func TestReleaseTagCaptureAllocationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, tag, asset string
		budget           float64
	}{
		{"平台标签", "windows-v1.2.3", "setup.exe", 0},
		{"平台别名", "ohos-1.2.3", "setup.hap", 0},
		{"版本含横线", "linux-v1.2.3-beta-4", "setup.deb", 0},
		{"混合大小写", "Windows-v1.2.3", "setup.exe", 1},
		{"产品标签回退", "msime-v1.2.3", "setup.apk", 0},
		{"未知平台", "unknown-v1.2.3", "source.tar.gz", 0},
		{"无平台标签", "v1.2.3", "setup.apk", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var platform, version string
			// 充分采样，避免竞态检测随机丢弃正则缓存影响整数均值。
			allocated := testing.AllocsPerRun(1000, func() {
				platform, version = releaseTagPlatform(tc.tag, tc.asset)
			})
			wantPlatform, wantVersion := releaseTagPlatformBeforeCaptures(tc.tag, tc.asset)
			if platform != wantPlatform || version != wantVersion {
				t.Fatal("发布标签的平台或版本改变")
			}
			if allocated > tc.budget {
				t.Fatalf("标签解析分配 %.0f 次，预算 %.0f 次", allocated, tc.budget)
			}
		})
	}
}

func TestReleaseTagCaptureCompatibility(t *testing.T) {
	check := func(tag, asset string) {
		t.Helper()
		platform, version := releaseTagPlatform(tag, asset)
		wantPlatform, wantVersion := releaseTagPlatformBeforeCaptures(tag, asset)
		if platform != wantPlatform || version != wantVersion {
			t.Fatalf("tag=%q，asset=%q：实际 (%q, %q)，期望 (%q, %q)", tag, asset, platform, version, wantPlatform, wantVersion)
		}
	}
	for _, prefix := range []string{"windows", "win", "macos", "mac", "osx", "linux", "android", "ios", "harmony", "harmonyos", "ohos", "Windows", "LINUX", "msime", "unknown", "p9", "9p", "", "win-dows", "win_dows", "İOS", "win\xff"} {
		for _, version := range []string{"1", "v1.2.3", "1.2.3-beta-4", "v9+build_2", "v1-", "V1", "v", "", "1/2", "1 2", "界", "1\n", "1\xff", strings.Repeat("1", 4096)} {
			for _, asset := range []string{"", "setup.exe", "setup.apk", "setup.APPIMAGE", "source.tar.gz", "setup.İPA", "setup.\xff"} {
				tag := prefix + "-" + version
				check(tag, asset)
				check(" "+tag+" ", asset)
			}
		}
	}
	random := rand.New(rand.NewPCG(71, 113))
	for range 1024 {
		raw := make([]byte, random.IntN(128))
		for i := range raw {
			raw[i] = byte(random.Uint32())
		}
		check(string(raw), "setup.exe")
		check("windows-v1-"+string(raw), "setup.apk")
		check("unknown-v1", string(raw))
	}
}

func BenchmarkReleaseTagCaptures(b *testing.B) {
	for _, tc := range []struct{ name, tag, asset string }{
		{"平台标签", "windows-v1.2.3", "setup.exe"},
		{"混合大小写", "Windows-v1.2.3", "setup.exe"},
		{"产品标签回退", "msime-v1.2.3", "setup.apk"},
		{"未知平台", "unknown-v1.2.3", "source.tar.gz"},
		{"无平台标签", "v1.2.3", "setup.apk"},
		{"汇总500项", "", "setup.exe"},
	} {
		tags := []string{tc.tag}
		if tc.name == "汇总500项" {
			tags = make([]string, downloadsRowLimit)
			for i := range tags {
				tags[i] = "windows-v1.2." + strconv.Itoa(i)
			}
		}
		for _, impl := range []struct {
			name     string
			classify func(string, string) (string, string)
		}{{"原实现", releaseTagPlatformBeforeCaptures}, {"当前", releaseTagPlatform}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				var wants [][2]string
				for _, tag := range tags {
					platform, version := releaseTagPlatformBeforeCaptures(tag, tc.asset)
					wants = append(wants, [2]string{platform, version})
				}
				b.ReportAllocs()
				for b.Loop() {
					for i, tag := range tags {
						platform, version := impl.classify(tag, tc.asset)
						if platform != wants[i][0] || version != wants[i][1] {
							b.Fatal("发布标签的平台或版本改变")
						}
					}
				}
			})
		}
	}
}

// 冻结原捕获切片解析，独立比较结果和分配，扩展名规则与本次优化前一致。
func releaseTagPlatformBeforeCaptures(tag, asset string) (string, string) {
	if m := releaseTag.FindStringSubmatch(tag); m != nil {
		if platform, ok := releaseTagPlatforms[strings.ToLower(m[1])]; ok {
			return platform, m[2]
		}
	}
	if extension := releaseAssetExtension(asset); extension != "" {
		switch extension {
		case ".exe", ".msi", ".msix", ".appx":
			return "windows", tag
		case ".dmg", ".pkg":
			return "macos", tag
		case ".deb", ".rpm", ".appimage", ".flatpak", ".snap":
			return "linux", tag
		case ".apk", ".aab":
			return "android", tag
		case ".ipa":
			return "ios", tag
		case ".hap", ".app":
			return "harmony", tag
		}
	}
	return "", tag
}
