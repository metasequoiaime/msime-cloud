package server

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

// qqReadReleases 共用最近一页 Release 的令牌、缓存读取和解码，保留上游顺序。
// 空列表和 null 都是成功读取，调用方再按用途选择版本。
func (s *Server) qqReadReleases(ctx context.Context, repo string) ([]qqRelease, bool) {
	client := s.adminGitHub
	token, err := client.Token(ctx, repo, qqGitHubReadPerms)
	if err != nil {
		slog.Warn("qq bot: release lookup skipped", "repo", repo, "reason", err.Error())
		return nil, false
	}
	r, err := client.Get(ctx, token, "/repos/"+repo+"/releases?per_page="+strconv.Itoa(qqReleasePage))
	var list []qqRelease
	if err != nil || !r.OK() || r.Decode(&list) != nil {
		slog.Warn("qq bot: release lookup failed", "repo", repo, "reason", githubFailure("releases", r, err).Error())
		return nil, false
	}
	return list, true
}

// qqReleases 读 repo 最近一页 Release（读请求带 ETag 缓存），按发布时间稳定倒序排列。
func (s *Server) qqReleases(ctx context.Context, repo string) ([]qqRelease, bool) {
	list, ok := s.qqReadReleases(ctx, repo)
	if !ok {
		return nil, false
	}
	// 修复版本以发布时间为准；列表位置不能代表版本实际发布的先后。
	slices.SortStableFunc(list, func(a, b qqRelease) int { return strings.Compare(b.PublishedAt, a.PublishedAt) })
	return list, true
}
