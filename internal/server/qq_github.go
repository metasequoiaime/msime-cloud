package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// 答疑时查 GitHub。群友常问「这个问题有人报过吗」「修好了没」「最新版本是多少」，文档答不了，所以挑段落那一步顺带让模型给出搜 issue 的关键词、判断要不要查版本，服务端用管理后台的 GitHub App 只读地查，把结果和文档段落一起交给回答的模型。查询失败只记日志，答疑照常进行。

const (
	// qqIssueResults 是每个仓库交给模型的 issue 数。
	qqIssueResults = 5
	// qqReleasePage 是查最新版本时每个仓库读的 Release 数；各平台在同一个仓库里交替发布，50 个足够覆盖每个前缀的最近一次。
	qqReleasePage = 50
)

var qqGitHubReadPerms = map[string]string{"contents": "read", "issues": "read", "metadata": "read"}

// qqGitHubRepos 是答疑时要查的仓库：qq_bot.repo 和 admin.github.platforms 里的各平台仓库，去重后按名字排序。
func (s *Server) qqGitHubRepos() []string {
	repos := []string{s.qq.config.Repo}
	for _, p := range s.config.Admin.GitHub.Platforms {
		if !slices.ContainsFunc(repos, func(r string) bool { return strings.EqualFold(r, p.Repo) }) {
			repos = append(repos, p.Repo)
		}
	}
	slices.Sort(repos)
	return repos
}

// qqIssueState 是 issue 状态的中文写法：已关闭的按 state_reason 区分已解决和不计划处理，因为关闭不一定代表修好了。
func qqIssueState(state, reason string) string {
	switch {
	case state == "open":
		return "开着"
	case reason == "completed":
		return "已解决"
	case reason == "not_planned":
		return "不计划处理"
	default:
		return "已关闭"
	}
}

// qqSearchIssues 在每个仓库里按关键词搜 issue（开着的和已关闭的都算，不含拉取请求），返回给模型看的文字；没有结果时为空。GitHub 要求每个词都匹配，模型多给一个词就可能一条都搜不到，所以搜不到时依次只用前两个、前一个词再搜。
func (s *Server) qqSearchIssues(ctx context.Context, keywords string) string {
	words := strings.Fields(qqSearchTerms(keywords))
	if len(words) == 0 {
		return ""
	}
	attempts := []string{strings.Join(words, " ")}
	for _, n := range []int{2, 1} {
		if len(words) > n {
			attempts = append(attempts, strings.Join(words[:n], " "))
		}
	}
	for _, terms := range attempts {
		if found := s.qqSearchIssuesFor(ctx, terms); found != "" {
			return found
		}
	}
	return ""
}

// qqSearchIssuesFor 是用一组搜索词做的一轮 qqSearchIssues。
func (s *Server) qqSearchIssuesFor(ctx context.Context, terms string) string {
	client := s.adminGitHub
	var b strings.Builder
	for _, repo := range s.qqGitHubRepos() {
		token, err := client.Token(ctx, repo, qqGitHubReadPerms)
		if err != nil {
			slog.Warn("qq bot: issue search skipped", "repo", repo, "reason", err.Error())
			continue
		}
		r, err := client.Do(ctx, token, "GET", "/search/issues?per_page="+strconv.Itoa(qqIssueResults)+"&q="+url.QueryEscape("repo:"+repo+" is:issue "+terms), nil)
		if err != nil || !r.OK() {
			slog.Warn("qq bot: issue search failed", "repo", repo, "reason", githubFailure("search", r, err).Error())
			continue
		}
		var found struct {
			Items []struct {
				qqIssue
				State       string `json:"state"`
				StateReason string `json:"state_reason"`
				UpdatedAt   string `json:"updated_at"`
				Labels      []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"items"`
		}
		if r.Decode(&found) != nil {
			continue
		}
		_, name, _ := strings.Cut(repo, "/")
		for _, issue := range found.Items {
			if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" || issue.Number <= 0 {
				continue
			}
			labels := []string{}
			for _, label := range issue.Labels {
				labels = append(labels, label.Name)
			}
			updated, _, _ := strings.Cut(issue.UpdatedAt, "T")
			fmt.Fprintf(&b, "- %s #%d（%s，更新于 %s，标签：%s）%s\n", name, issue.Number, qqIssueState(issue.State, issue.StateReason), updated, strings.Join(labels, "、"), issue.Title)
		}
	}
	return b.String()
}

// qqRelease 是 GitHub Release 列表里用到的字段。
type qqRelease struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
}

// qqLatestReleases 列出各平台的最新版本：按 admin.github.platforms 的仓库和 tag_prefix 找最新的正式版，以及比它更新的预发布版；平台仓库不是 qq_bot.repo 时，再到 qq_bot.repo 里找以「平台 id-v」开头的预览版（例如 msime 里的 windows-v）。
func (s *Server) qqLatestReleases(ctx context.Context) string {
	client := s.adminGitHub
	cache := map[string][]qqRelease{}
	// failed 记下查询失败的仓库：查不到不等于没发布，这些仓库的平台不写进结果。
	failed := map[string]bool{}
	releases := func(repo string) []qqRelease {
		if list, ok := cache[repo]; ok {
			return list
		}
		cache[repo] = nil
		failed[repo] = true
		token, err := client.Token(ctx, repo, qqGitHubReadPerms)
		if err != nil {
			slog.Warn("qq bot: release lookup skipped", "repo", repo, "reason", err.Error())
			return nil
		}
		r, err := client.Get(ctx, token, "/repos/"+repo+"/releases?per_page="+strconv.Itoa(qqReleasePage))
		var list []qqRelease
		if err != nil || !r.OK() || r.Decode(&list) != nil {
			slog.Warn("qq bot: release lookup failed", "repo", repo, "reason", githubFailure("releases", r, err).Error())
			return nil
		}
		cache[repo], failed[repo] = list, false
		return list
	}
	// latest 返回 repo 里前缀为 prefix 的最新正式版和更晚发布的预发布版（Release 列表按创建时间从新到旧）。
	latest := func(repo, prefix string) (stable, preview *qqRelease) {
		for i, release := range releases(repo) {
			if release.Draft || !strings.HasPrefix(release.TagName, prefix) {
				continue
			}
			version := release.TagName[len(prefix):]
			if version == "" || version[0] < '0' || version[0] > '9' {
				continue
			}
			list := cache[repo]
			if release.Prerelease {
				if preview == nil && stable == nil {
					preview = &list[i]
				}
				continue
			}
			stable = &list[i]
			return stable, preview
		}
		return stable, preview
	}
	date := func(r *qqRelease) string {
		d, _, _ := strings.Cut(r.PublishedAt, "T")
		return d
	}
	var b strings.Builder
	for _, p := range s.config.Admin.GitHub.Platforms {
		if p.TagPrefix == "" {
			continue
		}
		_, name, _ := strings.Cut(p.Repo, "/")
		stable, preview := latest(p.Repo, p.TagPrefix)
		if failed[p.Repo] {
			continue
		}
		parts := []string{}
		if stable != nil {
			parts = append(parts, fmt.Sprintf("正式版 %s（%s 发布）", stable.TagName, date(stable)))
		}
		if preview != nil {
			parts = append(parts, fmt.Sprintf("预发布 %s（%s 发布）", preview.TagName, date(preview)))
		}
		if !strings.EqualFold(p.Repo, s.qq.config.Repo) {
			stable, preview := latest(s.qq.config.Repo, p.ID+"-v")
			_, home, _ := strings.Cut(s.qq.config.Repo, "/")
			for _, r := range []*qqRelease{stable, preview} {
				if r != nil {
					parts = append(parts, fmt.Sprintf("%s 仓库的新版预览 %s（%s 发布）", home, r.TagName, date(r)))
				}
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "还没有发布")
		}
		fmt.Fprintf(&b, "- %s（%s 仓库）：%s\n", p.Name, name, strings.Join(parts, "；"))
	}
	return b.String()
}
