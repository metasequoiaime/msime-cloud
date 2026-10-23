package server

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
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

func releaseSearchCapacityServer(count int) *Server {
	s := &Server{}
	for platform := range 2 {
		p := AdminPlatformConfig{ID: "platform-" + strconv.Itoa(platform), Name: "合成平台"}
		s.config.Admin.GitHub.Platforms = append(s.config.Admin.GitHub.Platforms, p)
		var releases []ghRelease
		for i := platform; i < count; i += 2 {
			releases = append(releases, ghRelease{TagName: "v1.2." + strconv.Itoa(i)})
		}
		s.indexReleases(p, releases)
	}
	return s
}

func TestSearchReleasesMatchedAllocationBudget(t *testing.T) {
	for _, count := range []int{0, 1, 5, 8, 20} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := releaseSearchCapacityServer(count)
			want := previousSearchReleases(s, "v1.2.")
			var got []account.AdminSearchHit
			allocations := testing.AllocsPerRun(100, func() { got = s.searchReleases("v1.2.") })
			if !slices.Equal(got, want) || (got == nil) != (want == nil) {
				t.Fatalf("发布搜索结果或顺序变化：got=%#v want=%#v", got, want)
			}
			budget := 0.0
			if len(want) > 0 {
				budget = 1
			}
			if allocations > budget {
				t.Fatalf("命中发布搜索分配 %.0f 次，期望最多 %.0f 次", allocations, budget)
			}
			if len(got) > 0 {
				if cap(got) != len(got) {
					t.Fatalf("发布搜索结果容量未收紧：len=%d cap=%d", len(got), cap(got))
				}
				got[0].Title = "调用方修改结果"
				if again := s.searchReleases("v1.2."); !slices.Equal(again, want) {
					t.Fatalf("搜索结果修改影响后续查询：%#v", again)
				}
			}
		})
	}
}

func BenchmarkSearchReleasesMatchedCapacity(b *testing.B) {
	for _, count := range []int{0, 1, 5, 8, 20} {
		s := releaseSearchCapacityServer(count)
		for _, tc := range []struct {
			name string
			fn   func(*Server, string) []account.AdminSearchHit
		}{
			{"previous", previousSearchReleases},
			{"current", (*Server).searchReleases},
		} {
			b.Run(strconv.Itoa(count)+"/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if got := tc.fn(s, "v1.2."); len(got) != min(count, 8) {
						b.Fatal("命中发布结果数量变化")
					}
				}
			})
		}
	}
}

func previousSearchReleases(s *Server, q string) []account.AdminSearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	s.releaseIndex.mu.Lock()
	defer s.releaseIndex.mu.Unlock()
	var hits []account.AdminSearchHit
	for _, p := range s.config.Admin.GitHub.Platforms {
		for _, hit := range s.releaseIndex.byPlatform[p.ID] {
			if strings.Contains(hit.lowerTitle, q) || strings.Contains(hit.lowerTag, q) {
				hits = append(hits, hit.AdminSearchHit)
				if len(hits) == 8 {
					return hits
				}
			}
		}
	}
	return hits
}
