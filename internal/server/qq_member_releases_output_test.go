package server

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func previousQQReleasesForMembers(list []qqPlatformRelease) string {
	var b strings.Builder
	for _, p := range list {
		parts := []string{}
		if p.Stable != nil {
			parts = append(parts, previousQQMemberReleaseLabel(p.Stable, p.Prefix, ""))
		}
		if p.Preview != nil {
			parts = append(parts, "预览版 "+previousQQMemberReleaseLabel(p.Preview, p.Prefix, ""))
		}
		for _, r := range p.NextGen {
			parts = append(parts, "新版 "+p.Name+" 预览 "+previousQQMemberReleaseLabel(&r, p.NextGenPrefix, "测试中"))
		}
		if len(parts) == 0 {
			parts = append(parts, "还没有发布")
		}
		fmt.Fprintf(&b, "%s：%s\n", p.Name, strings.Join(parts, "｜"))
	}
	return b.String()
}

func previousQQMemberReleaseLabel(r *qqRelease, prefix, note string) string {
	var inside []string
	if len(r.PublishedAt) >= 10 {
		inside = append(inside, r.PublishedAt[5:10])
	}
	if note != "" {
		inside = append(inside, note)
	}
	label := strings.TrimPrefix(r.TagName, prefix)
	if len(inside) > 0 {
		label += "（" + strings.Join(inside, "，") + "）"
	}
	return label
}

func qqMemberReleasesOutputFixture() []qqPlatformRelease {
	list := benchmarkReleasePlatformList()
	for i := range list {
		p := &list[i]
		p.Prefix = "v"
		p.Preview = &qqRelease{TagName: "v0.11.0-beta.1", PublishedAt: "2026-10-12T00:00:00Z"}
		p.NextGenPrefix = p.ID + "-v"
		p.NextGen = []qqRelease{{TagName: p.NextGenPrefix + "0.2.1", PublishedAt: "2026-10-13T00:00:00Z"}}
	}
	return list
}

var qqMemberReleasesOutputResult string

func TestQQMemberReleasesOutputAllocations(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []qqPlatformRelease
		max  float64
	}{
		{"空列表", nil, 0},
		{"多平台多版本", qqMemberReleasesOutputFixture(), 1},
		{"尚未发布", []qqPlatformRelease{{Name: "Windows"}, {Name: "macOS"}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				qqMemberReleasesOutputResult = qqReleasesForMembers(tc.list)
			})
			if allocs > tc.max {
				t.Fatalf("分配 %.0f 次，期望最多 %.0f 次", allocs, tc.max)
			}
		})
	}
}

func TestQQMemberReleasesOutputCompatibility(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	names := []string{"", "Windows", "水杉平台", "平台%\n"}
	tags := []string{"", "v", "v0.10.1", "windows-v0.2.0", "预览版", "\xffv1"}
	prefixes := []string{"", "v", "windows-v", "不存在", "\xff"}
	dates := []string{"", "2026-10-2", "2026-10-23", "2026-10-23T00:00:00Z", "日期边界测试", "\xff2026-10-23"}
	for range 1000 {
		list := make([]qqPlatformRelease, rng.Intn(7))
		release := func() *qqRelease {
			return &qqRelease{TagName: tags[rng.Intn(len(tags))], PublishedAt: dates[rng.Intn(len(dates))]}
		}
		for i := range list {
			p := &list[i]
			p.Name = names[rng.Intn(len(names))]
			p.Prefix = prefixes[rng.Intn(len(prefixes))]
			p.NextGenPrefix = prefixes[rng.Intn(len(prefixes))]
			if rng.Intn(2) != 0 {
				p.Stable = release()
			}
			if rng.Intn(2) != 0 {
				p.Preview = release()
			}
			for range rng.Intn(4) {
				p.NextGen = append(p.NextGen, *release())
			}
		}
		if got, want := qqReleasesForMembers(list), previousQQReleasesForMembers(list); got != want {
			t.Fatalf("输出不一致：\n当前 %q\n原先 %q", got, want)
		}
	}
}

func BenchmarkQQMemberReleasesOutput(b *testing.B) {
	for _, tc := range []struct {
		name string
		list []qqPlatformRelease
	}{
		{"多平台多版本", qqMemberReleasesOutputFixture()},
		{"尚未发布", []qqPlatformRelease{{Name: "Windows"}, {Name: "macOS"}}},
	} {
		for _, impl := range []struct {
			name string
			run  func([]qqPlatformRelease) string
		}{
			{"修改前", previousQQReleasesForMembers},
			{"修改后", qqReleasesForMembers},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					qqMemberReleasesOutputResult = impl.run(tc.list)
				}
			})
		}
	}
}
