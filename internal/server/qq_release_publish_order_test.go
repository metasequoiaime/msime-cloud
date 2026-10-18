package server

import (
	"context"
	"encoding/json"
	"testing"
)

// 最新正式版和预发布版按发布时间挑选，不依赖 GitHub 列表中的位置。
func TestQQLatestReleasesUsesPublishTime(t *testing.T) {
	stable := qqRelease{TagName: "android-v1.0.0", PublishedAt: "2026-10-02T00:00:00Z"}
	newStable := qqRelease{TagName: "android-v1.1.0", PublishedAt: "2026-10-04T00:00:00Z"}
	oldPreview := qqRelease{TagName: "android-v1.0.0-beta", Prerelease: true, PublishedAt: "2026-10-01T00:00:00Z"}
	preview := qqRelease{TagName: "android-v1.1.0-beta", Prerelease: true, PublishedAt: "2026-10-03T00:00:00Z"}
	newPreview := qqRelease{TagName: "android-v1.2.0-beta", Prerelease: true, PublishedAt: "2026-10-05T00:00:00Z"}
	for _, tc := range []struct {
		name         string
		releases     []qqRelease
		stable, beta string
	}{
		{name: "旧正式版排在前面", releases: []qqRelease{stable, newStable}, stable: newStable.TagName},
		{name: "较晚发布的预发布版排在正式版之后", releases: []qqRelease{stable, preview}, stable: stable.TagName, beta: preview.TagName},
		{name: "较早发布的预发布版排在正式版之前", releases: []qqRelease{oldPreview, stable}, stable: stable.TagName},
		{name: "预发布版之间也按发布时间选择", releases: []qqRelease{preview, newPreview, stable}, stable: stable.TagName, beta: newPreview.TagName},
		{name: "已按发布时间排序", releases: []qqRelease{newPreview, newStable, stable, oldPreview}, stable: newStable.TagName, beta: newPreview.TagName},
		{name: "草稿和其他平台不参与选择", releases: []qqRelease{
			{TagName: "android-v2.0.0", Draft: true, PublishedAt: "2026-10-06T00:00:00Z"},
			{TagName: "windows-v2.0.0", PublishedAt: "2026-10-07T00:00:00Z"},
			stable, preview,
		}, stable: stable.TagName, beta: preview.TagName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			s.config.Admin.GitHub.Platforms = []AdminPlatformConfig{{ID: "android", Name: "Android", Repo: "o/r", TagPrefix: "android-v"}}
			body, err := json.Marshal(tc.releases)
			if err != nil {
				t.Fatal(err)
			}
			fake.releases = map[string]string{"o/r": string(body)}
			got := s.qqLatestReleases(context.Background())
			if len(got) != 1 {
				t.Fatalf("平台数=%d，期望 1", len(got))
			}
			stableTag, previewTag := "", ""
			if got[0].Stable != nil {
				stableTag = got[0].Stable.TagName
			}
			if got[0].Preview != nil {
				previewTag = got[0].Preview.TagName
			}
			if stableTag != tc.stable || previewTag != tc.beta {
				t.Fatalf("正式版=%q，预发布版=%q；期望 %q、%q", stableTag, previewTag, tc.stable, tc.beta)
			}
			fake.mu.Lock()
			calls := fake.releaseCalls
			fake.mu.Unlock()
			if calls != 1 {
				t.Fatalf("Release 请求数=%d，期望 1", calls)
			}
		})
	}
}

// 先创建的草稿可能更晚发布，首个修复版本应取关闭后最早发布的版本。
func TestQQFixReleaseUsesPublishTime(t *testing.T) {
	s, fake, _ := qqServer(t)
	fake.releases = map[string]string{"o/r": `[
		{"tag_name":"windows-v1.2.0","published_at":"2026-10-04T00:00:00Z"},
		{"tag_name":"windows-v1.0.0","published_at":"2026-10-02T00:00:00Z"},
		{"tag_name":"windows-v1.1.0","published_at":"2026-10-03T00:00:00Z"}
	]`}
	issue := qqIssueDetail{State: "closed", StateReason: "completed", ClosedAt: "2026-10-01T00:00:00Z"}
	issue.Labels = append(issue.Labels, struct {
		Name string `json:"name"`
	}{Name: qqWindowsLabel})
	want := "已修好，windows-v1.0.0 及之后的版本都有"
	if got := s.qqFixRelease(context.Background(), "o/r", issue); got != want {
		t.Fatalf("修复版本=%q，期望 %q", got, want)
	}
}
