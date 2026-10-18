package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// 满页中的最后一条记录可能属于其他平台、尚未发布或仍是草稿，不能据此认为修复版本的历史已读全。
func TestQQFixReleaseFilteredPageBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tail     qqRelease
		pageSize int
		want     string
	}{
		{
			name:     "末条属于其他平台",
			tail:     qqRelease{TagName: "android-v1.0.0", PublishedAt: "2026-10-02T00:00:00Z"},
			pageSize: qqReleasePage,
			want:     "已经修好（2026-10-01），更新到最新版就有",
		},
		{
			name:     "末条是草稿",
			tail:     qqRelease{TagName: "windows-v0.1.0", PublishedAt: "2026-09-30T00:00:00Z", Draft: true},
			pageSize: qqReleasePage,
			want:     "已经修好（2026-10-01），更新到最新版就有",
		},
		{
			name:     "末条没有发布时间",
			tail:     qqRelease{TagName: "windows-v0.1.0"},
			pageSize: qqReleasePage,
			want:     "已经修好（2026-10-01），更新到最新版就有",
		},
		{
			name:     "末条仍是关闭后的同平台版本",
			tail:     qqRelease{TagName: "windows-v0.1.0", PublishedAt: "2026-10-02T00:00:00Z"},
			pageSize: qqReleasePage,
			want:     "已经修好（2026-10-01），更新到最新版就有",
		},
		{
			name:     "已经读到关闭前的同平台版本",
			tail:     qqRelease{TagName: "windows-v0.1.0", PublishedAt: "2026-09-30T00:00:00Z"},
			pageSize: qqReleasePage,
			want:     "已修好，windows-v0.2.0 及之后的版本都有",
		},
		{
			name:     "不满页表示历史已经读全",
			tail:     qqRelease{TagName: "android-v1.0.0", PublishedAt: "2026-10-02T00:00:00Z"},
			pageSize: qqReleasePage - 1,
			want:     "已修好，windows-v0.2.0 及之后的版本都有",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			list := make([]qqRelease, tc.pageSize)
			for i := range list {
				list[i] = qqRelease{TagName: fmt.Sprintf("android-v1.0.%d", i), PublishedAt: "2026-10-03T00:00:00Z"}
			}
			list[0] = qqRelease{TagName: "windows-v0.2.0", PublishedAt: "2026-10-04T00:00:00Z"}
			list[len(list)-1] = tc.tail
			body, err := json.Marshal(list)
			if err != nil {
				t.Fatal(err)
			}
			fake.releases = map[string]string{"o/r": string(body)}
			issue := qqIssueDetail{State: "closed", StateReason: "completed", ClosedAt: "2026-10-01T00:00:00Z"}
			issue.Labels = append(issue.Labels, struct {
				Name string `json:"name"`
			}{Name: qqWindowsLabel})
			if got := s.qqFixRelease(context.Background(), "o/r", issue); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			fake.mu.Lock()
			calls := fake.releaseCalls
			fake.mu.Unlock()
			if calls != 1 {
				t.Fatalf("release requests = %d, want 1", calls)
			}
		})
	}
}
