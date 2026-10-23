package account

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// 独立保留原来的全文转小写与后缀规则，对照元数据资产的分类结果。
func previousReleaseMetadataAsset(asset string) bool {
	name := strings.ToLower(asset)
	for _, suffix := range []string{".sha256", ".sha512", ".sha256sum", ".sig", ".asc", ".minisig", ".txt", ".yml", ".yaml", ".json", ".blockmap", ".zsync"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func TestReleaseMetadataExtensionCompatibility(t *testing.T) {
	check := func(asset string) {
		t.Helper()
		if got, want := releaseMetadataAsset(asset), previousReleaseMetadataAsset(asset); got != want {
			t.Fatalf("元数据资产 %q 的分类改变：结果=%t，期望=%t", asset, got, want)
		}
	}
	for _, suffix := range []string{"", ".", ".exe", ".sha256", ".sha512", ".sha256sum", ".sig", ".asc", ".minisig", ".txt", ".yml", ".yaml", ".json", ".blockmap", ".zsync", ".mİnİsİg", ".blocKmap", ".ſig", ".json/child", ".json.", ".json\xff"} {
		for _, stem := range []string{"", "MSIME-1.2.3", "目录/安装包😀", "İΣ\xff", "../release."} {
			check(stem + suffix)
			check(stem + strings.ToUpper(suffix))
		}
	}
	random := rand.New(rand.NewPCG(19, 37))
	for range 1000 {
		data := make([]byte, random.IntN(512))
		for i := range data {
			data[i] = byte(random.IntN(256))
		}
		check(string(data))
		check(string(data) + ".JSON")
	}
}

func TestReleaseAssetStemAllocationBudget(t *testing.T) {
	for _, asset := range []string{"MSIME-Windows-X64.exe", "MSIME-latest.json", "MSIME-NIGHTLY", "MSIME-安装包😀.apk", "\xff.MSIME.json"} {
		t.Run(asset, func(t *testing.T) {
			wantPlatform, wantVersion := previousReleaseTagPlatform("nightly", asset)
			wantMetadata := previousReleaseMetadataAsset(asset)
			allocations := testing.AllocsPerRun(100, func() {
				platform, version := releaseTagPlatform("nightly", asset)
				if platform != wantPlatform || version != wantVersion || releaseMetadataAsset(asset) != wantMetadata {
					t.Fatal("资产平台、版本或元数据分类改变")
				}
			})
			if allocations != 0 {
				t.Fatalf("只含小写扩展名的资产分配 %.0f 次，期望无需复制文件名", allocations)
			}
		})
	}
}

func TestReleaseAssetUppercaseExtensionAllocationBudget(t *testing.T) {
	asset := strings.Repeat("MSIME-", 700) + ".APPIMAGE"
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			platform, version := releaseTagPlatform("nightly", asset)
			if platform != "linux" || version != "nightly" || releaseMetadataAsset(asset) {
				b.Fatal("长资产名分类改变")
			}
		}
	})
	t.Logf("长资产名分类：%s；%s", result, result.MemString())
	// 两个入口只需复制短扩展名；为 race 和进程背景分配留余量，仍排除整份长文件名复制。
	if bytes := result.AllocedBytesPerOp(); bytes > 1024 {
		t.Fatalf("长资产名分类分配 %d 字节，超过 1 KiB 预算", bytes)
	}
}

func BenchmarkReleaseAssetExtensionBuffer(b *testing.B) {
	for _, asset := range []string{"MSIME-Windows-X64.exe", "MSIME-latest.json", "MSIME-1.2.3.AppImage", strings.Repeat("MSIME-", 700) + ".APPIMAGE"} {
		name := asset
		if len(name) > 64 {
			name = "长资产名"
		}
		b.Run(name, func(b *testing.B) {
			wantPlatform, wantVersion := previousReleaseTagPlatform("nightly", asset)
			wantMetadata := previousReleaseMetadataAsset(asset)
			b.ReportAllocs()
			for b.Loop() {
				platform, version := releaseTagPlatform("nightly", asset)
				if platform != wantPlatform || version != wantVersion || releaseMetadataAsset(asset) != wantMetadata {
					b.Fatal("资产分类改变")
				}
			}
		})
	}
}
