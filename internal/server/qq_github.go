package server

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
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

// qqIssueState 是 issue 状态的中文写法：已关闭的按 state_reason 区分已完成和不计划处理，因为关闭不一定代表修好了；打了 invalid、duplicate、wontfix 标签的按标签说（这类 issue 也常以 completed 关闭）。msime 合进开发分支就关 issue，修复要等下一个版本才发布，所以 completed 写「已完成」，不写「已解决」。开着的写「还没解决」：群友关心的是修没修，「开着」是 GitHub 的说法，他们看不懂；也不写「还在处理」，开着不等于有人在处理。
func qqIssueState(state, reason string, labels []string) string {
	if state != "open" {
		for _, label := range labels {
			switch strings.ToLower(label) {
			case "invalid":
				return "已关闭（无效）"
			case "duplicate":
				return "已关闭（重复）"
			case "wontfix":
				return "已关闭（不修）"
			}
		}
	}
	switch {
	case state == "open":
		return "还没解决"
	case reason == "completed":
		return "已完成"
	case reason == "not_planned":
		return "不计划处理"
	default:
		return "已关闭"
	}
}

// qqIssueHit 是搜到的一个 issue：Name 是仓库短名，State 是 qqIssueState 的说法，UpdatedAt 只留日期；Extras 是补的上下文（见 qqIssueExtras），只有交给模型时才补。
type qqIssueHit struct {
	Name      string
	Number    int64
	Title     string
	State     string
	Labels    []string
	UpdatedAt string
	Extras    string
}

// qqSearchIssues 在每个仓库里按关键词搜 issue（开着的和已关闭的都算，不含拉取请求），返回给模型看的文字（见 qqIssueListForModel）；没有结果时为空。failed 的含义见 qqSearchIssueHits。
func (s *Server) qqSearchIssues(ctx context.Context, keywords string) (found string, failed bool) {
	hits, failed := s.qqSearchIssueHits(ctx, keywords, true)
	return qqIssueListForModel(hits), failed
}

// qqSearchIssueHits 是 qqSearchIssues 的结构化结果，「查」命令用它给群友另写一份短列表（见 qqIssueListForMembers），和给模型的资料不共用文字。extras 为 true 时给前 qqIssueContextResults 个 issue 补上下文。GitHub 要求每个词都匹配，模型多给一个词就可能一条都搜不到，所以搜不到时依次只用前两个、前一个词再搜。failed 表示有仓库没搜成（限流、GitHub 出错）：这时结果为空不等于没有相关 issue，也不再退词重搜，免得被限流时多打几轮请求。
func (s *Server) qqSearchIssueHits(ctx context.Context, keywords string, extras bool) (hits []qqIssueHit, failed bool) {
	words := strings.Fields(qqSearchTerms(keywords))
	if len(words) == 0 {
		return nil, false
	}
	attempts := []string{strings.Join(words, " ")}
	for _, n := range []int{2, 1} {
		if len(words) > n {
			attempts = append(attempts, strings.Join(words[:n], " "))
		}
	}
	for _, terms := range attempts {
		if hits, failed = s.qqSearchIssuesFor(ctx, terms, extras); len(hits) > 0 || failed {
			return hits, failed
		}
	}
	return nil, false
}

// qqSearchIssuesFor 是用一组搜索词做的一轮 qqSearchIssueHits。各仓库同时搜，都搜完后（extras 为 true 时）同时给前 qqIssueContextResults 个 issue（按仓库顺序数，见 qq_issue_context.go）补上下文，最后按仓库顺序排好：结果和逐个仓库查时一样，只是不再一个仓库搜完、补完上下文再搜下一个。
func (s *Server) qqSearchIssuesFor(ctx context.Context, terms string, extras bool) ([]qqIssueHit, bool) {
	client := s.adminGitHub
	type hit struct {
		qqIssue
		qqIssueDetail
		UpdatedAt string `json:"updated_at"`
	}
	repos := s.qqGitHubRepos()
	hits := make([][]hit, len(repos))
	failed := make([]bool, len(repos))
	var search errgroup.Group
	for i, repo := range repos {
		search.Go(func() error {
			token, err := client.Token(ctx, repo, qqGitHubReadPerms)
			if err != nil {
				slog.Warn("qq bot: issue search skipped", "repo", repo, "reason", err.Error())
				failed[i] = true
				return nil
			}
			r, err := client.Do(ctx, token, "GET", "/search/issues?per_page="+strconv.Itoa(qqIssueResults)+"&q="+url.QueryEscape("repo:"+repo+" is:issue "+terms), nil)
			if err != nil || !r.OK() {
				slog.Warn("qq bot: issue search failed", "repo", repo, "reason", githubFailure("search", r, err).Error())
				failed[i] = true
				return nil
			}
			var found struct {
				Items []hit `json:"items"`
			}
			if r.Decode(&found) != nil {
				failed[i] = true
				return nil
			}
			for _, issue := range found.Items {
				if len(issue.PullRequest) > 0 && string(issue.PullRequest) != "null" || issue.Number <= 0 {
					continue
				}
				hits[i] = append(hits[i], issue)
			}
			return nil
		})
	}
	_ = search.Wait()
	// contexts[i][j] 是第 i 个仓库第 j 个 issue 补的上下文；detailed 是已经安排补上下文的 issue 数。
	contexts := make([][]string, len(repos))
	var details errgroup.Group
	detailed := 0
	for i, repo := range repos {
		contexts[i] = make([]string, len(hits[i]))
		for j := range hits[i] {
			if !extras || detailed == qqIssueContextResults {
				break
			}
			detailed++
			details.Go(func() error {
				contexts[i][j] = s.qqIssueExtras(ctx, repo, hits[i][j].Number, hits[i][j].qqIssueDetail, qqMaintainerRunes, "  ")
				return nil
			})
		}
	}
	_ = details.Wait()
	var out []qqIssueHit
	for i, repo := range repos {
		_, name, _ := strings.Cut(repo, "/")
		for j, issue := range hits[i] {
			labels := issue.labelNames()
			updated, _, _ := strings.Cut(issue.UpdatedAt, "T")
			out = append(out, qqIssueHit{Name: name, Number: issue.Number, Title: issue.Title, State: qqIssueState(issue.State, issue.StateReason, labels), Labels: labels, UpdatedAt: updated, Extras: contexts[i][j]})
		}
	}
	return out, slices.Contains(failed, true)
}

// qqIssueListForModel 是交给模型的搜索结果：每个 issue 一行，带状态、更新日期和标签，补的上下文缩进写在下面。
func qqIssueListForModel(hits []qqIssueHit) string {
	var b strings.Builder
	for _, hit := range hits {
		fmt.Fprintf(&b, "- %s #%d（%s，更新于 %s，标签：%s）%s\n", hit.Name, hit.Number, hit.State, hit.UpdatedAt, strings.Join(hit.Labels, "、"), hit.Title)
		b.WriteString(hit.Extras)
	}
	return b.String()
}

// qqMemberIssueResults 是「查」命令回复群友时最多列的 issue 数。
const qqMemberIssueResults = 5

// qqIssueTitleTag 匹配 issue 标题开头的「[Bug]」「[Feature]:」这类模板前缀。
var qqIssueTitleTag = regexp.MustCompile(`^\s*\[[^\]]{1,20}\]\s*:?\s*`)

// qqIssueListForMembers 是「查」命令发到群里的短列表：最多 qqMemberIssueResults 条，每条一行「msime #7066 标题（还没解决）」，标题去掉模板前缀，不带标签、日期和维护者评论，最后一行告诉群友怎么看某条的进展。曾经直接发给模型看的资料，最多十行，带英文标签，外加三段几百字没清洗的维护者评论，在千人群里刷屏。
func qqIssueListForMembers(hits []qqIssueHit) string {
	var b strings.Builder
	for _, hit := range hits[:min(len(hits), qqMemberIssueResults)] {
		title := cmp.Or(strings.TrimSpace(qqIssueTitleTag.ReplaceAllString(hit.Title, "")), strings.TrimSpace(hit.Title))
		fmt.Fprintf(&b, "%s #%d %s（%s）\n", hit.Name, hit.Number, title, hit.State)
	}
	b.WriteString("以上是 GitHub 上搜到的相关 issue，想看某条的进展，@我 发「#编号」")
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

// qqStatsTTL 是仓库统计在本副本的缓存时间：每次统计要打十几次搜索接口，而 GitHub 的搜索接口每分钟只给 30 次。qqStatsParallel 是统计时每个仓库同时发的搜索请求数，不再加大，免得撞上 GitHub 对并发请求的次级限流。
const (
	qqStatsTTL      = 5 * time.Minute
	qqStatsParallel = 3
)

// qqStatsZone 是统计「今天」用的时区：群友在国内，按北京时间算。
var qqStatsZone = time.FixedZone("UTC+8", 8*3600)

// qqRepoStats 列出每个答疑仓库的 issue 和 PR 数，回答「msime 有多少 issue」「今天处理了多少」这类问题：开着的 issue、累计关闭的 issue、开着的 PR，以及今天和最近 7 天（北京时间）新开和关闭的 issue、合并的 PR。用搜索接口的 total_count（每个数一次请求，per_page=1）；某个数查不到时那个仓库整行不写，不写成 0。结果缓存 qqStatsTTL。
func (s *Server) qqRepoStats(ctx context.Context) string {
	b := s.qq
	b.mu.Lock()
	if b.stats != "" && b.now().Sub(b.statsAt) < qqStatsTTL {
		stats := b.stats
		b.mu.Unlock()
		return stats
	}
	b.mu.Unlock()
	now := b.now().In(qqStatsZone)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, qqStatsZone)
	day, week := today.Format("2006-01-02T15:04:05-07:00"), today.AddDate(0, 0, -6).Format("2006-01-02T15:04:05-07:00")
	queries := []string{"is:issue is:open", "is:issue is:closed", "is:pr is:open",
		"is:issue created:>=" + day, "is:issue closed:>=" + day, "is:pr is:merged merged:>=" + day,
		"is:issue created:>=" + week, "is:issue closed:>=" + week, "is:pr is:merged merged:>=" + week}
	// 各仓库同时查，每个仓库里最多 qqStatsParallel 个查询同时发；某个数查不到时取消这个仓库剩下的查询（曾经串行时也是查不到就停），只记第一个失败的原因。曾经两个仓库 18 次搜索逐个发，冷缓存时要七到十秒。
	client := s.adminGitHub
	repos := s.qqGitHubRepos()
	lines := make([]string, len(repos))
	var all errgroup.Group
	for i, repo := range repos {
		all.Go(func() error {
			token, err := client.Token(ctx, repo, qqGitHubReadPerms)
			if err != nil {
				slog.Warn("qq bot: repo stats skipped", "repo", repo, "reason", err.Error())
				return nil
			}
			counts := make([]int, len(queries))
			g, qctx := errgroup.WithContext(ctx)
			g.SetLimit(qqStatsParallel)
			for k, q := range queries {
				g.Go(func() error {
					if err := qctx.Err(); err != nil {
						return err
					}
					r, err := client.Get(qctx, token, "/search/issues?per_page=1&q="+url.QueryEscape("repo:"+repo+" "+q))
					var found struct {
						TotalCount *int `json:"total_count"`
					}
					if err != nil || !r.OK() || r.Decode(&found) != nil || found.TotalCount == nil {
						return githubFailure("stats", r, err)
					}
					counts[k] = *found.TotalCount
					return nil
				})
			}
			if err := g.Wait(); err != nil {
				slog.Warn("qq bot: repo stats failed", "repo", repo, "reason", err.Error())
				return nil
			}
			_, name, _ := strings.Cut(repo, "/")
			lines[i] = fmt.Sprintf("- %s：现在开着的 issue %d 个，建仓以来累计关闭的 issue %d 个，开着的 PR %d 个；今天（北京时间 %s）新开 issue %d 个、关闭 issue %d 个、合并 PR %d 个；最近 7 天新开 issue %d 个、关闭 issue %d 个、合并 PR %d 个\n",
				name, counts[0], counts[1], counts[2], today.Format("2006-01-02"), counts[3], counts[4], counts[5], counts[6], counts[7], counts[8])
			return nil
		})
	}
	_ = all.Wait()
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(line)
	}
	stats := out.String()
	if stats != "" {
		b.mu.Lock()
		b.stats, b.statsAt = stats, b.now()
		b.mu.Unlock()
	}
	return stats
}
