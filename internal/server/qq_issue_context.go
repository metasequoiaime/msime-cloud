package server

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// 答疑时给 issue 补上下文。群友最常问「修好了吗」，只看 issue 关没关不够：msime 合进开发分支就关 issue，修复要等下一个版本才发布；维护者在 issue 评论里写过的临时办法和用法说明也只在评论里。所以搜到的前几个 issue 和用户提到的 issue 都补两样：按关闭时间和该平台的 Release 判断修复有没有发版，以及维护者（OWNER、MEMBER、COLLABORATOR）最近的一条评论。

const (
	// qqIssueContextResults 是搜索结果里补上下文的 issue 数。
	qqIssueContextResults = 3
	// qqMaintainerRunes、qqMaintainerShortRunes 是交给模型的和直接回复群友的维护者评论长度上限。
	qqMaintainerRunes      = 600
	qqMaintainerShortRunes = 150
)

// qqImagePattern 匹配 Markdown 图片和 HTML 标签，维护者评论里的截图对模型没有用。
var qqImagePattern = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|<[^>\n]+>`)

// qqIssueDetail 是补上下文要用到的 issue 字段，和 qqIssue 一起嵌入解码（编号在 qqIssue 里，这里不能再有同名字段，否则两个都解不出来）。
type qqIssueDetail struct {
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	ClosedAt    string `json:"closed_at"`
	Comments    int    `json:"comments"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (d qqIssueDetail) labelNames() []string {
	names := make([]string, 0, len(d.Labels))
	for _, label := range d.Labels {
		names = append(names, label.Name)
	}
	return names
}

// qqReleaseSource 返回 repo 里一个 issue 对应平台的 Release 所在仓库和标签前缀：平台仓库（例如 msime-windows）里的 issue 就是那个平台；qq_bot.repo 里按 platform/ 标签找平台配置，platform/windows 是新实现，用 qq_bot.repo 里的「windows-v」。看不出平台时 ok 为 false。
func (s *Server) qqReleaseSource(repo string, labels []string) (releaseRepo, prefix string, ok bool) {
	if !strings.EqualFold(repo, s.qq.config.Repo) {
		for _, p := range s.config.Admin.GitHub.Platforms {
			if strings.EqualFold(p.Repo, repo) && p.TagPrefix != "" {
				return p.Repo, p.TagPrefix, true
			}
		}
		return "", "", false
	}
	if slices.Contains(labels, qqWindowsLabel) {
		return s.qq.config.Repo, "windows-v", true
	}
	for _, p := range s.config.Admin.GitHub.Platforms {
		if p.Label != "" && slices.Contains(labels, p.Label) && strings.EqualFold(p.Repo, repo) && p.TagPrefix != "" {
			return p.Repo, p.TagPrefix, true
		}
	}
	return "", "", false
}

// qqFixRelease 说明一个已完成的 issue 的修复有没有发版：关闭之后发布的最早的 Release 就是第一个包含修复的版本；关闭之后还没有发布过就是已合入、尚未发版。不是以 completed 关闭、打了无效类标签或看不出平台时返回空字符串。
func (s *Server) qqFixRelease(ctx context.Context, repo string, issue qqIssueDetail) string {
	labels := issue.labelNames()
	if issue.State != "closed" || issue.StateReason != "completed" || issue.ClosedAt == "" || qqIssueState(issue.State, issue.StateReason, labels) != "已完成" {
		return ""
	}
	releaseRepo, prefix, ok := s.qqReleaseSource(repo, labels)
	if !ok {
		return ""
	}
	releases, ok := s.qqReleases(ctx, releaseRepo)
	if !ok {
		return ""
	}
	var first *qqRelease
	matched := 0
	for i, release := range releases {
		if release.Draft || release.PublishedAt == "" || !strings.HasPrefix(release.TagName, prefix) {
			continue
		}
		matched++
		// Release 列表从新到旧，GitHub 的时间都是 UTC 的 RFC 3339，可以直接按字符串比较。
		if release.PublishedAt > issue.ClosedAt {
			first = &releases[i]
		}
	}
	closed, _, _ := strings.Cut(issue.ClosedAt, "T")
	switch {
	case matched == 0:
		return ""
	case first == nil:
		return "修复已合入（" + closed + " 关闭），之后还没有发布新版本"
	case len(releases) >= qqReleasePage && first == &releases[len(releases)-1]:
		// 只读了最近一页 Release，关闭时间比这一页都早时说不准是哪个版本。
		return "修复在 " + closed + " 合入，已经包含在较早发布的版本里"
	default:
		suffix := ""
		if first.Prerelease {
			suffix = "（预发布）"
		}
		return "修复已包含在 " + first.TagName + suffix + " 及之后的版本"
	}
}

// qqReleases 读 repo 最近一页 Release（读请求带 ETag 缓存）。
func (s *Server) qqReleases(ctx context.Context, repo string) ([]qqRelease, bool) {
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

// qqMaintainerComment 返回维护者在 issue 下最近的一条评论（去掉链接、图片和多余空白，截到 limit 字）；没有评论或读取失败时为空。
func (s *Server) qqMaintainerComment(ctx context.Context, repo string, number int64, issue qqIssueDetail, limit int) string {
	if issue.Comments == 0 {
		return ""
	}
	client := s.adminGitHub
	token, err := client.Token(ctx, repo, qqGitHubReadPerms)
	if err != nil {
		return ""
	}
	// 评论按时间从旧到新，最多读最近的 100 条：从最后一页读起。
	page := (issue.Comments + 99) / 100
	r, err := client.Get(ctx, token, "/repos/"+repo+"/issues/"+strconv.FormatInt(number, 10)+"/comments?per_page=100&page="+strconv.Itoa(page))
	var comments []struct {
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
	}
	if err != nil || !r.OK() || r.Decode(&comments) != nil {
		slog.Warn("qq bot: comment lookup failed", "repo", repo, "reason", githubFailure("comments", r, err).Error())
		return ""
	}
	for i := len(comments) - 1; i >= 0; i-- {
		switch comments[i].AuthorAssociation {
		case "OWNER", "MEMBER", "COLLABORATOR":
		default:
			continue
		}
		text := strings.Join(strings.Fields(qqStripLinks(qqImagePattern.ReplaceAllString(comments[i].Body, " "))), " ")
		if text == "" {
			continue
		}
		if runes := []rune(text); len(runes) > limit {
			text = string(runes[:limit]) + "…"
		}
		return text
	}
	return ""
}

// qqIssueExtras 是一个 issue 补充的上下文文字，每样一行，前面带 prefix；没有可补充的时为空。
func (s *Server) qqIssueExtras(ctx context.Context, repo string, number int64, issue qqIssueDetail, limit int, prefix string) string {
	var b strings.Builder
	if release := s.qqFixRelease(ctx, repo, issue); release != "" {
		b.WriteString(prefix + release + "\n")
	}
	if comment := s.qqMaintainerComment(ctx, repo, number, issue, limit); comment != "" {
		b.WriteString(prefix + "维护者最近说：" + comment + "\n")
	}
	return b.String()
}
