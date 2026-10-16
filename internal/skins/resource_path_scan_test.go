package skins

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var previousSkinResourcePattern = regexp.MustCompile(`^[a-zA-Z0-9._/-]{1,256}$`)

// 固定原有两套路径校验，避免对照实现随生产校验一起变化。
func previousClientSafeResource(name string, max int) bool {
	if name == "" || len(name) > max || strings.ContainsRune(name, '\\') {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func previousSkinSafeResource(name string) bool {
	if !previousSkinResourcePattern.MatchString(name) || !fs.ValidPath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return mediaType(name) != ""
}

func TestSkinResourcePathScanCompatibility(t *testing.T) {
	check := func(name string) {
		t.Helper()
		for _, max := range []int{-1, 0, 1, len(name) - 1, len(name), len(name) + 1, 128, 256, 512, int(^uint(0) >> 1)} {
			want := previousClientSafeResource(name, max)
			if got := clientSafeResource(name, max); got != want {
				t.Fatalf("客户端路径 %q，上限 %d：%t，期望 %t", name, max, got, want)
			}
			if got := SafeResource(name, max); got != want {
				t.Fatalf("公开路径校验 %q，上限 %d：%t，期望 %t", name, max, got, want)
			}
		}
		if got, want := safeResource(name), previousSkinSafeResource(name); got != want {
			t.Fatalf("目录路径 %q：%t，期望 %t", name, got, want)
		}
	}
	for _, name := range []string{"", ".", "..", "...", "/", "skin.toml", "SKIN.TOML", "images/a.PNG", "a.gif", "fonts/a.ttf", "images/a.png/", "images//a.png", "./a.png", "a/../b.png", "a/./b.png", "C:/a.png", `images\a.png`, "a\x00.png", "a\xff.png", "中文/a.png", "é/a.png", "a.png\n", "a\nb.png", "％２ｅ/a.png", strings.Repeat("a", 5000)} {
		check(name)
	}
	for _, length := range []int{127, 128, 129, 255, 256, 257, 511, 512, 513} {
		check(strings.Repeat("a", length-4) + ".png")
		check(strings.Repeat("a/", (length-5)/2) + "a.png")
	}
	for _, first := range []string{"", ".", "..", "...", "a", "A0_-", ".hidden", "x.y", "a b", `x\y`, "é", "\xff", "x\n"} {
		for _, second := range []string{"", ".", "..", "...", "a", "A0_-", ".hidden", "x.y", "a b", `x\y`, "é", "\xff", "x\n"} {
			for _, file := range []string{"skin.toml", "preview.png", "preview.PNG", "x.jpeg", "x.gif", "font.ttf", "style.css", "x.woff2", "x.txt", "", ".", ".."} {
				check(first + "/" + second + "/" + file)
			}
		}
	}
	// 在目录名、分隔符和文件名的每个位置覆盖全部字节。
	name := []byte("Images/preview.png")
	for pos, original := range name {
		for value := 0; value < 256; value++ {
			name[pos] = byte(value)
			check(string(name))
		}
		name[pos] = original
	}
}

func skinResourcePathScanInputs(kind string) ([]string, int, int) {
	names := make([]string, 1000)
	client, catalog := 0, 0
	for i := range names {
		switch kind {
		case "flat":
			names[i] = fmt.Sprintf("preview_%04d.png", i)
		case "nested":
			names[i] = fmt.Sprintf("Images/synthetic_%04d/preview.png", i)
		case "deep":
			names[i] = strings.Repeat("assets/", 16) + fmt.Sprintf("preview_%04d.png", i)
		default:
			switch i % 4 {
			case 0:
				names[i] = fmt.Sprintf("Images/synthetic_%04d/preview.png", i)
			case 1:
				names[i] = fmt.Sprintf("Images/synthetic_%04d/preview.PNG", i)
			case 2:
				names[i] = fmt.Sprintf("../escape_%04d.png", i)
			default:
				names[i] = fmt.Sprintf("Images/synthetic_%04d/preview.gif", i)
			}
		}
		if previousClientSafeResource(names[i], 256) {
			client++
		}
		if previousSkinSafeResource(names[i]) {
			catalog++
		}
	}
	return names, client, catalog
}

func skinResourcePathValidators() []struct {
	name  string
	check func(string) bool
} {
	return []struct {
		name  string
		check func(string) bool
	}{
		{"client", func(name string) bool { return SafeResource(name, 256) }},
		{"catalog", safeResource},
	}
}

func TestSkinResourcePathScanAllocationBudget(t *testing.T) {
	for _, validator := range skinResourcePathValidators() {
		for _, kind := range []string{"flat", "nested", "deep", "mixed"} {
			t.Run(validator.name+"/"+kind, func(t *testing.T) {
				names, want, catalog := skinResourcePathScanInputs(kind)
				budget := 0.0
				if validator.name == "catalog" {
					want = catalog
					if kind == "mixed" {
						// 保留 250 个大写扩展名转换所必需的分配。
						budget = 250
					}
				}
				accepted := 0
				allocations := testing.AllocsPerRun(10, func() {
					accepted = 0
					for _, name := range names {
						if validator.check(name) {
							accepted++
						}
					}
				})
				if accepted != want {
					t.Fatalf("接受 %d 个路径，期望 %d", accepted, want)
				}
				if allocations > budget {
					t.Fatalf("1000 个路径分配 %.0f 次，预算 %.0f 次", allocations, budget)
				}
				t.Logf("1000 个路径校验：%.0f 次分配", allocations)
			})
		}
	}
}

func BenchmarkSkinResourcePathScan(b *testing.B) {
	for _, validator := range skinResourcePathValidators() {
		for _, kind := range []string{"flat", "nested", "deep", "mixed"} {
			b.Run(validator.name+"/"+kind, func(b *testing.B) {
				names, want, catalog := skinResourcePathScanInputs(kind)
				if validator.name == "catalog" {
					want = catalog
				}
				b.ReportAllocs()
				for b.Loop() {
					accepted := 0
					for _, name := range names {
						if validator.check(name) {
							accepted++
						}
					}
					if accepted != want {
						b.Fatalf("接受 %d 个路径，期望 %d", accepted, want)
					}
				}
			})
		}
	}
}
