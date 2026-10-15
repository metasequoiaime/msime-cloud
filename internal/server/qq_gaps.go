package server

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 答不上的问题汇总。知识库和 GitHub 里都找不到答案时，回答的模型顺带用一句话概括群友想知道什么（不含个人信息），服务端把它作为评论追加到 qq_bot.gaps_repo 里一个固定的开放 issue，维护者照着补 msime-docs；补进文档后机器人就能回答，要人工回答的问题越来越少。qq_bot.gaps_repo 为空时不记录。群友报告的问题（答疑时模型判断是在报 bug 或提需求，escalate）也以「群友报告问题：」记到同一个 issue，每天另有 qqReportsDaily 条的额度，不挤占文档空缺。同一个概括 7 天内只记一次，每天最多 qqGapsDaily 条（auth_rates，所有副本共享）。先查每天的额度再占 7 天去重的名额：反过来的话，当天额度用完时去重名额也被占掉，这个问题 7 天内都记不下来。回复群友时按是否真的记下选说法，没记下不说「已经记下来了」。

const (
	qqGapsTitle    = "QQ 机器人答不上的问题"
	qqGapsBody     = "QQ 群答疑机器人在文档和 GitHub 里都没找到答案的问题，由机器人转述后追加在下面，不包含群友的原话和身份。\n\n把答案补进 msime-docs 的 guides/ 后，机器人最多 10 分钟就能回答。处理完的评论可以隐藏，全部处理完可以关闭本 issue，下一个答不上的问题会自动开一个新的。"
	qqGapsDaily    = 50
	qqReportsDaily = 50
	// qqGapRunes 是一条概括的长度上限。
	qqGapRunes = 200
	// qqGapsIssueTTL 是汇总 issue 编号的缓存时间：维护者关闭它后，最多这么久之后新的问题会记到新开的 issue 上，不会一直评论在已关闭的 issue 下。
	qqGapsIssueTTL = time.Hour
	// qqGapRecordTimeout 是记一条汇总的时限（见 recordQQGapIn）。要拿令牌、查汇总 issue、可能新建、再评论，几次 GitHub 调用，3 秒不够；也不能长：副本关停时 Close 等满 qqCloseGrace 取消 lifetime 之后还要等它，preStop、HTTP 关停、qqCloseGrace 加上它要落在部署的 terminationGracePeriodSeconds（100 秒）以内。
	qqGapRecordTimeout = 10 * time.Second
)

// qqContactPatterns 是写进汇总 issue 前要删掉的联系方式：手机号（可带 +86）、邮箱，以及「QQ / 群 / 微信」后面跟着的号码。概括是模型对群友原话的转述，提示词要求不写个人信息，但代码以前不检查：群友不小心带上的手机号会被照抄出去，「提文档 FAQ 应写上官方售后 QQ 12345678」这类话也会把联系方式发到公开仓库。微信号可以是字母开头的串，只在写明「微信号」或带冒号时才删，免得把「微信 windows版」这类正常说法删掉。
var qqContactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`),
	regexp.MustCompile(`\+?\b(?:86[- ]?)?1[3-9]\d{9}\b`),
	regexp.MustCompile(`(?i)(?:qq|扣扣|群|微信|vx|wx)\s*(?:号码|号)?\s*[:：]?\s*\d{5,}`),
	regexp.MustCompile(`(?i)(?:微信|vx|wx)(?:\s*(?:号码|号)\s*[:：]?|\s*[:：])\s*[a-z][-_a-z0-9]{5,19}`),
}

// qqContactRedacted 代替删掉的联系方式，维护者看得出这里原来有东西。
const qqContactRedacted = "（已隐去）"

// qqRedactContacts 删掉 text 里的联系方式，见 qqContactPatterns。
func qqRedactContacts(text string) string {
	for _, pattern := range qqContactPatterns {
		text = pattern.ReplaceAllString(text, qqContactRedacted)
	}
	return text
}

// qqAppLoginTimeout 是启动时取 App 的 bot 账号的时限。
const qqAppLoginTimeout = 15 * time.Second

// qqAppLogin 返回管理后台 GitHub App 的 bot 账号「<slug>[bot]」：用 installation token 写的 issue 和评论，作者都是这个账号。启动时取一次（warmQQAppLogin），取到后缓存在本副本；没取到时下次用到再取。
func (s *Server) qqAppLogin(ctx context.Context) (string, error) {
	b := s.qq
	b.mu.Lock()
	login := b.appLogin
	b.mu.Unlock()
	if login != "" {
		return login, nil
	}
	r, err := s.adminGitHub.App(ctx)
	var app struct {
		Slug string `json:"slug"`
	}
	if err != nil || !r.OK() || r.Decode(&app) != nil || app.Slug == "" {
		return "", githubFailure("app", r, err)
	}
	login = app.Slug + "[bot]"
	b.mu.Lock()
	b.appLogin = login
	b.mu.Unlock()
	return login, nil
}

// warmQQAppLogin 在启动时取一次 App 的 bot 账号，失败只记日志。
func (s *Server) warmQQAppLogin(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, qqAppLoginTimeout)
	defer cancel()
	if _, err := s.qqAppLogin(ctx); err != nil {
		slog.Warn("qq bot: app login not loaded", "reason", err.Error())
	}
}

// qqWrittenByApp 判断一个 issue 或一条评论是不是机器人自己写的：performed_via_github_app 是管理后台的 App，或者作者是它的 bot 账号。汇总 issue 和计数评论以前只看标题和内容，外人新开一个同名 issue 就能把答不上的问题都引到他那里，留一条带计数标记的评论就能让机器人改写它、在群里念出假的人数。known 为 false 表示 performed_via_github_app 没写、bot 账号又没取到，判断不了。
func (s *Server) qqWrittenByApp(ctx context.Context, author qqGitHubAuthor) (owned, known bool) {
	if author.App != nil {
		return author.App.ID == s.adminGitHub.AppID, true
	}
	login, err := s.qqAppLogin(ctx)
	if err != nil {
		slog.Warn("qq bot: app login unavailable", "reason", err.Error())
		return false, false
	}
	return strings.EqualFold(author.User.Login, login), true
}

// qqGapsIssue 返回 gaps_repo 里标题为 qqGapsTitle、由机器人自己建的开放 issue 编号，没有时新建。编号缓存在本副本；新建时持跨副本锁，免得两个副本各建一个。分不清同名 issue 是不是机器人建的（bot 账号没取到）时返回错误，不新建：多半是机器人自己那个，再建一个就重复了。
func (s *Server) qqGapsIssue(ctx context.Context, token string) (int64, error) {
	b := s.qq
	b.mu.Lock()
	number := b.gapsIssue
	if b.now().Sub(b.gapsIssueAt) >= qqGapsIssueTTL {
		number = 0
	}
	b.mu.Unlock()
	if number > 0 {
		return number, nil
	}
	if s.qqLocks != nil {
		release, err := s.qqLocks.WaitLock(ctx, "qq-gaps-issue", 15*time.Second)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	client, repo := s.adminGitHub, s.qq.config.GapsRepo
	unknown := false
	for page := 1; ; page++ {
		r, err := client.Do(ctx, token, "GET", "/repos/"+repo+"/issues?state=open&per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil || !r.OK() {
			return 0, githubFailure("gaps list", r, err)
		}
		var issues []qqIssue
		if r.Decode(&issues) != nil {
			return 0, errors.New("gaps list: invalid response")
		}
		for _, issue := range issues {
			if issue.Title != qqGapsTitle || len(issue.PullRequest) != 0 && string(issue.PullRequest) != "null" {
				continue
			}
			owned, known := s.qqWrittenByApp(ctx, issue.qqGitHubAuthor)
			if owned {
				number = issue.Number
				break
			}
			unknown = unknown || !known
		}
		// 一页写满时继续查找，避免长期保留的汇总被后来的 Issue 挤出第一页后重复新建。
		if number > 0 || len(issues) < 100 {
			break
		}
	}
	if number == 0 && unknown {
		return 0, errors.New("gaps list: app login unavailable")
	}
	if number == 0 {
		r, err := client.Do(ctx, token, "POST", "/repos/"+repo+"/issues", map[string]any{"title": qqGapsTitle, "body": qqGapsBody})
		var created struct {
			Number int64 `json:"number"`
		}
		if err != nil || !r.OK() || r.Decode(&created) != nil || created.Number <= 0 {
			return 0, githubFailure("gaps create", r, err)
		}
		number = created.Number
		client.Invalidate("/repos/" + repo + "/issues")
	}
	b.mu.Lock()
	b.gapsIssue, b.gapsIssueAt = number, b.now()
	b.mu.Unlock()
	return number, nil
}

// recordQQGap 把一个答不上的问题（模型的概括）追加到汇总 issue，prefix 是评论开头，例如「群友问：」。返回是否已经记下（7 天内记过的同一个概括也算记下）；失败只记日志，并退回去重名额，下次遇到同一个问题会重新写。
func (s *Server) recordQQGap(ctx context.Context, prefix, topic string) bool {
	return s.recordQQGapIn(ctx, "qq-bot-gaps-day", qqGapsDaily, prefix, topic)
}

// recordQQReport 把群友在答疑里报告的问题（模型的「平台 + 现象」概括）追加到汇总 issue，用单独的每日额度。
func (s *Server) recordQQReport(ctx context.Context, topic string) bool {
	return s.recordQQGapIn(ctx, "qq-bot-reports-day", qqReportsDaily, "群友报告问题：", topic)
}

// recordQQGapIn 是 recordQQGap 和 recordQQReport 的实现，dailyScope、daily 是每天的额度。它是答疑和「提文档」的收尾，不跟随处理消息的 ctx（写法照 recordQQTurn）：答案已经拿到，滚动发布取消了 ctx 也要记下，回复才能照实说「已经记下来了」；另限 qqGapRecordTimeout。
func (s *Server) recordQQGapIn(ctx context.Context, dailyScope string, daily int, prefix, topic string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqGapRecordTimeout)
	defer cancel()
	// 联系方式在去重和截断之前删掉：7 天去重的键和写进 issue 的是同一段文字。
	topic = qqRedactContacts(strings.Join(strings.Fields(topic), " "))
	switch {
	case s.qq.config.GapsRepo == "":
		slog.Info("qq bot: gap skipped", "reason", "no_repo")
		return false
	case topic == "":
		slog.Warn("qq bot: gap skipped", "reason", "empty_topic")
		return false
	}
	if runes := []rune(topic); len(runes) > qqGapRunes {
		topic = string(runes[:qqGapRunes])
	}
	for _, limit := range []struct {
		scope, subject string
		count          int
		window         time.Duration
	}{{dailyScope, s.qq.config.GapsRepo, daily, 24 * time.Hour}, {"qq-bot-gap", topic, 1, 7 * 24 * time.Hour}} {
		switch err := s.limits.RateLimit(ctx, limit.scope, limit.subject, limit.count, limit.window); {
		case errors.Is(err, account.ErrLimited) && limit.scope == "qq-bot-gap":
			return true
		case errors.Is(err, account.ErrLimited):
			slog.Info("qq bot: gap skipped", "reason", "daily_cap", "scope", dailyScope)
			return false
		case err != nil:
			slog.Warn("qq bot: gap not recorded", "reason", err.Error())
			return false
		}
	}
	// 去重名额在写 GitHub 之前就占了；后面任何一步失败都要退回，否则 7 天内再遇到同一个问题会被当成已经记下，回复「已经记下来了」，其实汇总 issue 里没有。每天的额度不退。ctx 这时可能已经失效，退名额用单独的时限。
	release := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.limits.ReleaseRateLimit(rctx, "qq-bot-gap", topic); err != nil {
			slog.Warn("qq bot: gap slot not released", "reason", err.Error())
		}
	}
	client, repo := s.adminGitHub, s.qq.config.GapsRepo
	token, err := client.Token(ctx, repo, issueTokenPerms)
	if err != nil {
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		release()
		return false
	}
	number, err := s.qqGapsIssue(ctx, token)
	if err != nil {
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		release()
		return false
	}
	r, err := client.Do(ctx, token, "POST", "/repos/"+repo+"/issues/"+strconv.FormatInt(number, 10)+"/comments", map[string]string{"body": prefix + githubReferenceBreaker.Replace(topic)})
	if err != nil || !r.OK() {
		// 汇总 issue 可能已经被关闭或删除；清掉缓存，下一次重新查找或新建。
		s.qq.mu.Lock()
		s.qq.gapsIssue = 0
		s.qq.mu.Unlock()
		slog.Warn("qq bot: gap not recorded", "reason", githubFailure("gaps comment", r, err).Error())
		release()
		return false
	}
	client.Invalidate("/repos/" + repo + "/issues")
	return true
}
