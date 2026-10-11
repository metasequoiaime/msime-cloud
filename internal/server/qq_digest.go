package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// 管理员周报。每条消息处理完按天、按处理结果计数（account 的 qq_daily_stats，所有副本共享），管理员发「周报」一次看完最近 7 天：收到多少、答上率、答错几次、待提交的草稿、文档缺口、这周从 QQ 提了几个 issue，不用一个个命令去翻。

// qqStatsStore 是 account.Service 里的处理结果计数；没有数据库时为 nil。
type qqStatsStore interface {
	IncrQQOutcome(ctx context.Context, day time.Time, outcome string) error
	QQOutcomes(ctx context.Context, since time.Time) (map[string]int, error)
}

// recordQQOutcome 给今天（北京时间）的处理结果计数，出错只记日志。
func (s *Server) recordQQOutcome(ctx context.Context, outcome string) {
	if s.qqStats == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.qqStats.IncrQQOutcome(ctx, s.qq.now().In(qqStatsZone), outcome); err != nil {
		slog.Warn("qq bot: outcome not counted", "reason", err.Error())
	}
}

// qqOutcomeKind 把处理结果归成周报里的几类。
func qqOutcomeKind(outcome string) string {
	outcome = strings.TrimPrefix(strings.TrimPrefix(outcome, "admin_"), "retry_")
	switch {
	case strings.HasPrefix(outcome, "answered"):
		return "answered"
	case strings.HasPrefix(outcome, "unknown"):
		return "unknown"
	case outcome == "chat":
		return "chat"
	case strings.HasPrefix(outcome, "cmd_"):
		return "command"
	case strings.HasPrefix(outcome, "limited_"):
		return "limited"
	// timeout 是 4 分钟的处理时间用完（见 handleQQFeedback），和 failed 一样算出错。
	case outcome == "failed" || outcome == "timeout":
		return "failed"
	// reply_failed 是回答写好了却没发出去（见 handleQQFeedback），多半是被开放平台拒收，单独列出：多了说明答案里有内容被平台拦，要改的是提示词而不是上游。
	case outcome == "reply_failed":
		return "reply_failed"
	default:
		return "other"
	}
}

// qqWeeklyDigest 回复最近 7 天的周报。
func (s *Server) qqWeeklyDigest(ctx context.Context, reply func(string)) {
	now := s.qq.now().In(qqStatsZone)
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, qqStatsZone).AddDate(0, 0, -6)
	var b strings.Builder
	fmt.Fprintf(&b, "最近 7 天（%s 到 %s）：", since.Format("01-02"), now.Format("01-02"))
	if s.qqStats != nil {
		outcomes, err := s.qqStats.QQOutcomes(ctx, since)
		if err != nil {
			slog.Warn("qq bot: outcomes unavailable", "reason", err.Error())
		} else {
			kinds := map[string]int{}
			total := 0
			for outcome, n := range outcomes {
				kinds[qqOutcomeKind(outcome)] += n
				total += n
			}
			rate := "—"
			if asked := kinds["answered"] + kinds["unknown"]; asked > 0 {
				rate = fmt.Sprintf("%d%%", kinds["answered"]*100/asked)
			}
			fmt.Fprintf(&b, "\n- 收到 %d 条 @：答上 %d、答不上 %d（答上率 %s）、闲聊 %d、命令 %d、限流 %d、出错 %d、回复没发出去 %d", total, kinds["answered"], kinds["unknown"], rate, kinds["chat"], kinds["command"], kinds["limited"], kinds["failed"], kinds["reply_failed"])
		}
	}
	if s.qqBadAnswers != nil {
		if list, err := s.qqBadAnswers.RecentQQBadAnswers(ctx, 7*24*time.Hour, 1000); err == nil {
			fmt.Fprintf(&b, "\n- 群友说答错 %d 次（发「差评」看）", len(list))
		}
	}
	if s.qqDrafts != nil {
		if drafts, err := s.qqDrafts.OpenQQDrafts(ctx, 1000); err == nil {
			fmt.Fprintf(&b, "\n- 待提交的草稿 %d 份（发「草稿」看）", len(drafts))
		}
	}
	if s.qqFAQCandidates != nil {
		if list, err := s.qqFAQCandidates.OpenQQFAQCandidates(ctx, 1000); err == nil {
			fmt.Fprintf(&b, "\n- 文档缺口 %d 条（发「文档缺口」看）", len(list))
		}
	}
	if filed, ok := s.qqIssuesFiledSince(ctx, since); ok {
		fmt.Fprintf(&b, "\n- 这周从 QQ 提的 issue %d 个", filed)
	}
	reply(b.String())
}

// qqIssuesFiledSince 数 since 以来各答疑仓库里带 qq_bot.labels 第一个标签（生产是 source/qq）的 issue；没配标签或查询失败时 ok 为 false。
func (s *Server) qqIssuesFiledSince(ctx context.Context, since time.Time) (int, bool) {
	if len(s.qq.config.Labels) == 0 {
		return 0, false
	}
	client := s.adminGitHub
	total := 0
	for _, repo := range s.qqGitHubRepos() {
		token, err := client.Token(ctx, repo, qqGitHubReadPerms)
		if err != nil {
			return 0, false
		}
		q := "repo:" + repo + " is:issue label:\"" + s.qq.config.Labels[0] + "\" created:>=" + since.Format("2006-01-02T15:04:05-07:00")
		r, err := client.Get(ctx, token, "/search/issues?per_page=1&q="+url.QueryEscape(q))
		var found struct {
			TotalCount *int `json:"total_count"`
		}
		if err != nil || !r.OK() || r.Decode(&found) != nil || found.TotalCount == nil {
			return 0, false
		}
		total += *found.TotalCount
	}
	return total, true
}
