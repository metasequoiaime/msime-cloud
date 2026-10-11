package server

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 答不上的问题汇总。知识库和 GitHub 里都找不到答案时，回答的模型顺带用一句话概括群友想知道什么（不含个人信息），服务端把它作为评论追加到 qq_bot.gaps_repo 里一个固定的开放 issue，维护者照着补 msime-docs；补进文档后机器人就能回答，要人工回答的问题越来越少。qq_bot.gaps_repo 为空时不记录。同一个概括 7 天内只记一次，每天最多 qqGapsDaily 条（auth_rates，所有副本共享）。

const (
	qqGapsTitle = "QQ 机器人答不上的问题"
	qqGapsBody  = "QQ 群答疑机器人在文档和 GitHub 里都没找到答案的问题，由机器人转述后追加在下面，不包含群友的原话和身份。\n\n把答案补进 msime-docs 的 guides/ 后，机器人最多 10 分钟就能回答。处理完的评论可以隐藏，全部处理完可以关闭本 issue，下一个答不上的问题会自动开一个新的。"
	qqGapsDaily = 50
	// qqGapRunes 是一条概括的长度上限。
	qqGapRunes = 200
	// qqGapsIssueTTL 是汇总 issue 编号的缓存时间：维护者关闭它后，最多这么久之后新的问题会记到新开的 issue 上，不会一直评论在已关闭的 issue 下。
	qqGapsIssueTTL = time.Hour
)

// qqGapsIssue 返回 gaps_repo 里标题为 qqGapsTitle 的开放 issue 编号，没有时新建。编号缓存在本副本；新建时持跨副本锁，免得两个副本各建一个。
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
	r, err := client.Do(ctx, token, "GET", "/repos/"+repo+"/issues?state=open&per_page=100", nil)
	if err != nil || !r.OK() {
		return 0, githubFailure("gaps list", r, err)
	}
	var issues []qqIssue
	if r.Decode(&issues) != nil {
		return 0, errors.New("gaps list: invalid response")
	}
	for _, issue := range issues {
		if issue.Title == qqGapsTitle && (len(issue.PullRequest) == 0 || string(issue.PullRequest) == "null") {
			number = issue.Number
			break
		}
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
	}
	b.mu.Lock()
	b.gapsIssue, b.gapsIssueAt = number, b.now()
	b.mu.Unlock()
	return number, nil
}

// recordQQGap 把一个答不上的问题（模型的概括）追加到汇总 issue。失败只记日志。
func (s *Server) recordQQGap(ctx context.Context, topic string) {
	topic = strings.Join(strings.Fields(topic), " ")
	if s.qq.config.GapsRepo == "" || topic == "" {
		return
	}
	if runes := []rune(topic); len(runes) > qqGapRunes {
		topic = string(runes[:qqGapRunes])
	}
	for _, limit := range []struct {
		scope, subject string
		count          int
		window         time.Duration
	}{{"qq-bot-gap", topic, 1, 7 * 24 * time.Hour}, {"qq-bot-gaps-day", s.qq.config.GapsRepo, qqGapsDaily, 24 * time.Hour}} {
		switch err := s.limits.RateLimit(ctx, limit.scope, limit.subject, limit.count, limit.window); {
		case errors.Is(err, account.ErrLimited):
			return
		case err != nil:
			slog.Warn("qq bot: gap not recorded", "reason", err.Error())
			return
		}
	}
	client, repo := s.adminGitHub, s.qq.config.GapsRepo
	token, err := client.Token(ctx, repo, issueTokenPerms)
	if err != nil {
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		return
	}
	number, err := s.qqGapsIssue(ctx, token)
	if err != nil {
		slog.Warn("qq bot: gap not recorded", "reason", err.Error())
		return
	}
	r, err := client.Do(ctx, token, "POST", "/repos/"+repo+"/issues/"+strconv.FormatInt(number, 10)+"/comments", map[string]string{"body": "群友问：" + githubReferenceBreaker.Replace(topic)})
	if err != nil || !r.OK() {
		// 汇总 issue 可能已经被关闭或删除；清掉缓存，下一次重新查找或新建。
		s.qq.mu.Lock()
		s.qq.gapsIssue = 0
		s.qq.mu.Unlock()
		slog.Warn("qq bot: gap not recorded", "reason", githubFailure("gaps comment", r, err).Error())
	}
}
