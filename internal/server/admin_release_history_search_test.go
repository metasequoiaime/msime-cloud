package server

import (
	"strconv"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestReleaseSearchTargetsAreInHistory(t *testing.T) {
	for _, count := range []int{0, 50, 51, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s, _ := adminRBACFixture(t)
			s.config.Admin.GitHub.Platforms = releaseTestPlatforms[:1]
			f := newFakeReleaseGitHub(t)
			s.adminGitHub = f.client(t)
			platform := releaseTestPlatforms[0]
			releases := make([]map[string]any, 100)
			for i := range releases {
				prefix := platform.TagPrefix
				if i >= count {
					prefix = "other-v"
				}
				stamp := time.Date(2026, 10, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
				releases[i] = map[string]any{
					"id": i + 1, "tag_name": prefix + "1." + strconv.Itoa(i) + ".0", "name": "", "body": "### 修复\n- 合成发布说明",
					"draft": i%3 == 1, "prerelease": i%3 == 2, "created_at": stamp, "published_at": stamp,
					"html_url": "https://example.test/releases/" + strconv.Itoa(i), "author": map[string]any{"login": "synthetic-maintainer"}, "assets": []any{},
				}
			}
			f.mu.Lock()
			f.releases[platform.Repo] = releases
			f.mu.Unlock()
			readonly := account.AdminTokenPrefix + "readonly"
			// 平台列表先建立完整搜索索引，模拟用户先看到搜索结果再进入发布历史。
			serveAdminJSON(t, s, adminRequest(s, "GET", "/api/releases", "", readonly), 200, nil)
			var history struct {
				Releases []releaseView `json:"releases"`
			}
			serveAdminJSON(t, s, adminRequest(s, "GET", "/api/releases/windows", "", readonly), 200, &history)
			visible := make(map[string]bool, len(history.Releases))
			for i, release := range history.Releases {
				visible[release.Tag] = true
				want := platform.TagPrefix + "1." + strconv.Itoa(count-1-i) + ".0"
				if release.Tag != want {
					t.Fatalf("发布历史筛选或排序错误：第 %d 项为 %q，期望 %q", i, release.Tag, want)
				}
			}
			for i := range count {
				tag := platform.TagPrefix + "1." + strconv.Itoa(i) + ".0"
				hits := s.searchReleases(tag)
				if len(hits) != 1 || hits[0].ID != platform.ID+":"+tag {
					t.Fatalf("搜索未返回目标版本 %q：%+v", tag, hits)
				}
				if !visible[tag] {
					t.Fatalf("搜索结果 %q 不在发布历史中，历史只有 %d 项", hits[0].ID, len(history.Releases))
				}
			}
			if len(history.Releases) != count {
				t.Fatalf("发布历史返回 %d 项，期望完整返回 %d 项", len(history.Releases), count)
			}
			if hits := s.searchReleases("other-v"); len(hits) != 0 {
				t.Fatalf("搜索混入其他平台的版本：%+v", hits)
			}
			if calls := f.count("GET /repos/" + platform.Repo + "/releases"); calls != 1 {
				t.Fatalf("历史读取未复用列表缓存，GitHub 共被调用 %d 次", calls)
			}
		})
	}
}
