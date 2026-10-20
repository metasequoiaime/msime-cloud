package server

import (
	"strconv"
	"testing"
)

func TestSearchReleasesUnmatchedAllocationBudget(t *testing.T) {
	s := &Server{}
	for platform := range 3 {
		p := AdminPlatformConfig{ID: "platform-" + strconv.Itoa(platform), Name: "平台 " + strconv.Itoa(platform)}
		s.config.Admin.GitHub.Platforms = append(s.config.Admin.GitHub.Platforms, p)
		releases := make([]ghRelease, 1000)
		for i := range releases {
			releases[i].TagName = "v1.2." + strconv.Itoa(i)
		}
		s.indexReleases(p, releases)
	}

	allocations := testing.AllocsPerRun(20, func() {
		if hits := s.searchReleases("does-not-match"); hits != nil {
			t.Fatalf("未命中搜索返回了结果：%v", hits)
		}
	})
	if allocations != 0 {
		t.Fatalf("未命中发布搜索分配 %.0f 次，期望零分配", allocations)
	}
}

func BenchmarkSearchReleasesUnmatched(b *testing.B) {
	s := &Server{}
	for platform := range 10 {
		p := AdminPlatformConfig{ID: "platform-" + strconv.Itoa(platform), Name: "平台 " + strconv.Itoa(platform)}
		s.config.Admin.GitHub.Platforms = append(s.config.Admin.GitHub.Platforms, p)
		releases := make([]ghRelease, 1000)
		for i := range releases {
			releases[i].TagName = "v1.2." + strconv.Itoa(i)
		}
		s.indexReleases(p, releases)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = s.searchReleases("does-not-match")
	}
}
