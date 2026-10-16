package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// 文档缺口自动起草 FAQ PR。答不上的「文档没写」问题原来只记进 msime-docs 的汇总 issue 和每周的改进依据，还要有人照着写文档，一直没人写，机器人能答的也就一直没变多。所以每周开改进依据的同时，挑上一周「文档没写」里出现至少 qqDocsDraftMinCount 次的话题（最多 qqDocsDraftWeekly 个），让机器人强制现查文档和 GitHub 再答一遍，答得上就走「记录答案」同一套流程在 msime-docs 开 FAQ PR，PR 正文写明答案没有经过人工核对；维护者核对合并就行，不对就关掉。答不上的下周再试。

const (
	qqDocsDraftWeekly   = 3
	qqDocsDraftMinCount = 2
	// qqDocsDraftWindow 是同一个话题起草过以后多久不再起草：PR 还开着或者被关掉了，都不该每周再开一个。
	qqDocsDraftWindow = 60 * 24 * time.Hour
	// qqDocsDraftTimeout 是一周起草的总时限：每个话题现查加整理、翻译、开 PR 大约一两分钟。
	qqDocsDraftTimeout = 10 * time.Minute
)

// qqDraftDocsForGaps 为 [from, to) 里「文档没写」的常见话题起草 FAQ PR，返回开出的 PR，写成 GitHub 会自动链接的「owner/repo#12「话题」」（话题里的 @ 和 # 已经打断），由 qqImprovementTick 在每周一调用。
func (s *Server) qqDraftDocsForGaps(ctx context.Context, from, to time.Time) []string {
	if s.qqImprovements == nil || s.qq.config.KnowledgeRepo == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqDocsDraftTimeout)
	defer cancel()
	events, err := s.qqImprovements.QQImprovementEvents(ctx, from, to)
	if err != nil {
		slog.Warn("qq bot: docs drafts skipped", "reason", err.Error())
		return nil
	}
	var topics []string
	for _, e := range events {
		if e.Kind == "unknown" && e.Detail == "docs" && strings.TrimSpace(e.Topic) != "" {
			topics = append(topics, e.Topic)
		}
	}
	var pulls []string
	tried := 0
	for _, c := range qqCountTopics(topics) {
		if c.count < qqDocsDraftMinCount || tried == qqDocsDraftWeekly {
			break
		}
		key := qqCompactLower(c.topic)
		if err := s.limits.RateLimit(ctx, "qq-bot-docs-draft", key, 1, qqDocsDraftWindow); err != nil {
			continue
		}
		tried++
		answer, err := s.qqAnswerQuestion(ctx, nil, c.topic, qqAsk{force: true})
		if err != nil || answer.Type != "answered" || strings.TrimSpace(answer.Answer) == "" || answer.Escalate {
			// 现查也答不上：退回名额，下周再试。
			s.qqReleaseDocsDraft(ctx, key)
			continue
		}
		why := fmt.Sprintf("QQ 群里上周有 %d 次问到「%s」，机器人答不上（文档里没写）。", c.count, githubReferenceBreaker.Replace(c.topic))
		if number := s.writeQQFAQ(ctx, "问："+c.topic+"\n答："+answer.Answer, why, func(string) {}); number > 0 {
			pulls = append(pulls, fmt.Sprintf("%s#%d「%s」", s.qq.config.KnowledgeRepo, number, githubReferenceBreaker.Replace(qqStripLinks(c.topic))))
			continue
		}
		s.qqReleaseDocsDraft(ctx, key)
	}
	if len(pulls) > 0 {
		slog.Info("qq bot: docs drafts opened", "count", len(pulls))
	}
	return pulls
}

// qqReleaseDocsDraft 退回一个话题的起草名额，出错只记日志。
func (s *Server) qqReleaseDocsDraft(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.limits.ReleaseRateLimit(ctx, "qq-bot-docs-draft", key); err != nil {
		slog.Warn("qq bot: docs draft slot not released", "reason", err.Error())
	}
}
