package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

// 功能需求自动聚合。群友提的需求多半已经有人提过；原来每次都整理成新草稿，管理员还得一个个判断是不是重复。现在群友提需求时，先按提 issue 时的查重（qqDuplicate：关键词搜开着的 issue，再由模型判断是不是同一件事）在对应仓库里找；找到了就不起草稿，在那个 issue 下维护一条计数评论「QQ 群里有 N 位群友需要这个功能」（只改这一条，不刷屏），并告诉群友已经有人提过、现在什么状态。同一个群友对同一个需求只算一次（auth_rates，30 天），这一票没记上时退回名额；改计数时持跨副本锁。找不到时照旧起草稿。

const (
	// qqVoteWindow 是同一个群友对同一个需求只算一次的时长。
	qqVoteWindow = 30 * 24 * time.Hour
	// qqVoteMarker 是计数评论末尾的标记，里面带着当前人数。
	qqVoteMarker = "<!-- qq-votes:"
)

var qqVotePattern = regexp.MustCompile(`<!-- qq-votes:([0-9]{1,7}) -->`)

type qqVoteComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	qqGitHubAuthor
}

// qqVoteWriteUncertainError 表示 GitHub 写入的结果未知：请求可能已经生效，只是响应没有可靠地返回。
type qqVoteWriteUncertainError struct{ cause error }

func (e *qqVoteWriteUncertainError) Error() string { return e.cause.Error() }
func (e *qqVoteWriteUncertainError) Unwrap() error { return e.cause }

// qqVoteBody 是有 n 位群友需要时的计数评论。
func qqVoteBody(n int) string {
	return fmt.Sprintf("QQ 群里有 %d 位群友需要这个功能（由 QQ 群答疑机器人统计，同一位群友只算一次）。%s%d -->", n, qqVoteMarker, n)
}

// qqVoteFeature 在已有的同一个需求下记一票，返回答案发出后单独回复的说明；没找到同一个需求、仓库要追问版本或者出错时返回空字符串，调用方照旧起草稿。
func (s *Server) qqVoteFeature(ctx context.Context, subject string, report qqReport, text string) string {
	repo, _, question := s.qqTarget(ctx, report, text, false)
	if question != "" {
		return ""
	}
	client := s.adminGitHub
	token, err := client.Token(ctx, repo, issueTokenPerms)
	if err != nil {
		slog.Warn("qq bot: vote skipped", "reason", err.Error())
		return ""
	}
	number, err := s.qqDuplicate(ctx, client, token, repo, report)
	if err != nil || number <= 0 {
		if err != nil {
			slog.Warn("qq bot: vote search failed", "reason", err.Error())
		}
		return ""
	}
	ref := qqIssueNumberText(repo, number)
	voter := subject + "|" + repo + "#" + strconv.FormatInt(number, 10)
	release, err := s.qqReserveDedupe(ctx, "qq-bot-vote", voter, qqVoteWindow)
	switch {
	case errors.Is(err, account.ErrLimited):
		return "这个需求已经有人提过：" + ref + "，你之前已经记过一票了。" + s.qqProgressHint(repo, number)
	case err != nil:
		slog.Warn("qq bot: vote limit unavailable", "reason", err.Error())
		return ""
	}
	count, err := s.qqAddVote(ctx, token, repo, number)
	if err != nil {
		slog.Warn("qq bot: vote not recorded", "repo", repo, "reason", err.Error())
		var uncertain *qqVoteWriteUncertainError
		if !errors.As(err, &uncertain) {
			// 确认写入没有发出或被 GitHub 拒绝时退回名额；否则这位群友 30 天内再提会被告知「你之前已经记过一票了」，计数评论里其实没有他。
			release()
		} else {
			slog.Warn("qq bot: vote write result uncertain; keeping vote slot", "repo", repo)
		}
		return "这个需求已经有人提过：" + ref + "，开发者会在那里跟进。" + s.qqProgressHint(repo, number)
	}
	return fmt.Sprintf("这个需求已经有人提过：%s，我帮你在那里记了一票，现在 QQ 群里一共 %d 人需要。%s", ref, count, s.qqProgressHint(repo, number))
}

// qqAddVote 把 issue 下机器人自己发的计数评论加一（没有就新建），返回新的人数。外人留的带计数标记的评论直接跳过（见 qqWrittenByApp）；分不清带标记的评论是谁发的（bot 账号没取到）时返回错误，不新建，免得同一个 issue 下出现两条计数。
func (s *Server) qqAddVote(ctx context.Context, token, repo string, number int64) (int, error) {
	if s.qqLocks != nil {
		release, err := s.qqLocks.WaitLock(ctx, "qq-vote:"+repo+"#"+strconv.FormatInt(number, 10), 15*time.Second)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	client := s.adminGitHub
	issuePath := "/repos/" + repo + "/issues/" + strconv.FormatInt(number, 10)
	var comments []qqVoteComment
	readComments := func(path string) (githubComments []qqVoteComment, response githubapp.Response, err error) {
		response, err = client.Do(ctx, token, "GET", path, nil)
		if err != nil || !response.OK() || response.Decode(&githubComments) != nil || githubComments == nil {
			return nil, response, githubFailure("vote comments", response, err)
		}
		return githubComments, response, nil
	}
	comments, r, err := readComments(issuePath + "/comments?per_page=100&page=1")
	if err != nil {
		return 0, err
	}
	if last := issueLastPage(r.Header); last > 1 {
		for page := 2; page <= last; page++ {
			batch, _, err := readComments(issuePath + "/comments?per_page=100&page=" + strconv.Itoa(page))
			if err != nil {
				return 0, err
			}
			comments = append(comments, batch...)
		}
	}
	unknown := false
	for _, c := range comments {
		m := qqVotePattern.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		owned, known := s.qqWrittenByApp(ctx, c.qqGitHubAuthor)
		if !owned {
			unknown = unknown || !known
			continue
		}
		n, _ := strconv.Atoi(m[1])
		// 写入结果可能在响应返回前丢失；先失效缓存，避免不确定时继续读到旧评论。
		defer client.Invalidate("/repos/" + repo + "/issues")
		r, err := qqWriteIssue(ctx, client, token, repo, "PATCH", "/repos/"+repo+"/issues/comments/"+strconv.FormatInt(c.ID, 10), map[string]string{"body": qqVoteBody(n + 1)})
		if err != nil || !r.OK() {
			failure := githubFailure("vote update", r, err)
			if err != nil || r.Status == 0 || r.Status >= 500 {
				return 0, &qqVoteWriteUncertainError{cause: failure}
			}
			return 0, failure
		}
		return n + 1, nil
	}
	if unknown {
		return 0, errors.New("vote comments: app login unavailable")
	}
	defer client.Invalidate("/repos/" + repo + "/issues")
	r, err = qqWriteIssue(ctx, client, token, repo, "POST", issuePath+"/comments", map[string]string{"body": qqVoteBody(1)})
	if err != nil || !r.OK() {
		failure := githubFailure("vote create", r, err)
		if err != nil || r.Status == 0 || r.Status >= 500 {
			return 0, &qqVoteWriteUncertainError{cause: failure}
		}
		return 0, failure
	}
	return 1, nil
}
