package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// qqDatabaseWriteDefinitelyRejected 只把 PostgreSQL 明确返回的语句错误视为未写入。连接中断、超时等错误的提交结果可能未知，不能退款后让已经写入的候选再次出现。
func qqDatabaseWriteDefinitelyRejected(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr)
}

// 把答案沉淀进文档。机器人现查 GitHub 才答上的问题，说明文档里缺这块；答不上的问题更是。这些都记成「文档候选」（account.QQFAQCandidate，编号 F12 这样），管理员发「文档缺口」看列表，「看 F12」看完整的问题和机器人当时的回答，「写进FAQ F12」用「记录答案」的流程（qq_faq.go）整理成 FAQ 的一条给知识库开 PR，没答上的在后面补上答案：「写进FAQ F12 答：……」；「忽略 F12」拿掉。同一个问题 7 天内只记一次（auth_rates）。写进 FAQ 前先在数据库里认领候选（state 改成 writing），两个管理员同时发只有一个开 PR；没开成时放回，放回不受处理消息的时限影响；副本中途被杀掉来不及放回时，认领超过 5 分钟（account.QQFAQCandidateClaimTimeout）就当作被打断，可以再处理。差评（qq_bad_answers.go，编号 B7 这样）共用「看」「写进FAQ」「忽略」这几个命令。

const (
	// qqFAQCandidateList 是「文档缺口」列出的条数，qqFAQCandidateRunes 是候选里问题和答案的长度上限，也是「看 F编号」回复里问题和答案各自的长度上限。
	qqFAQCandidateList  = 10
	qqFAQCandidateRunes = 600
	// qqFAQCandidateStoreTimeout 是开完 PR 后标记候选、或者没开成时放回认领的时限。
	qqFAQCandidateStoreTimeout = 5 * time.Second
)

// qqFAQCandidateStore 是 account.Service 里的文档候选，所有副本共享；没有数据库时为 nil。
type qqFAQCandidateStore interface {
	AddQQFAQCandidate(ctx context.Context, question, answer string) (int64, error)
	QQFAQCandidate(ctx context.Context, id int64) (account.QQFAQCandidate, error)
	OpenQQFAQCandidates(ctx context.Context, limit int) ([]account.QQFAQCandidate, int, error)
	ClaimQQFAQCandidateLease(ctx context.Context, id int64) (time.Time, bool, error)
	ReleaseQQFAQCandidateLease(ctx context.Context, id int64, claimedAt time.Time) (bool, error)
	CloseQQFAQCandidateLease(ctx context.Context, id int64, claimedAt time.Time, state, ref string) (bool, error)
	CloseQQFAQCandidate(ctx context.Context, id int64, state, ref string) (bool, error)
}

// qqFAQWriteCommand 匹配「写进FAQ F12 …」「写进FAQ B7 …」，分组是 F 或 B、编号和后面的内容；qqFAQIgnoreCommand 匹配「忽略 F12」「忽略 B7」，qqFAQViewCommand 匹配「看 F12」「看 B7」，分组是 F 或 B 和编号。动词也认「写入」「加进」「加到」「放进」「记进」，也认命令后的冒号、编号前的「#」和字母与数字之间的空格：原来管理员写成「写进FAQ：F3」「写入FAQ F3」就被当成反馈去整理，F3 没写进 FAQ。
var (
	qqFAQWriteCommand  = regexp.MustCompile(`(?i)^(?:写进|写入|加进|加到|放进|记进)\s*faq\s*[:：]?\s*[#＃]?([fb])\s*([0-9]{1,9})(?:[\s:：,，。.;；、]+([\s\S]*))?$`)
	qqFAQIgnoreCommand = regexp.MustCompile(`(?i)^忽略\s*[:：]?\s*[#＃]?([fb])\s*([0-9]{1,9})(?:[\s:：,，。.;；、]+[\s\S]*)?$`)
	qqFAQViewCommand   = regexp.MustCompile(`(?i)^看\s*[:：]?\s*[#＃]?([fb])\s*([0-9]{1,9})$`)
)

// qqAnswerPrefix 是「写进FAQ F12 答：……」里答案的开头。
var qqAnswerPrefix = regexp.MustCompile(`^答[:：]\s*`)

// recordQQFAQCandidate 记下一条文档候选；同一个问题 7 天内只记一次，数据库明确拒绝时退回去重额度，结果不确定时保留额度。在回复之后调用，不跟随处理消息的 ctx、另限 3 秒（写法照 recordQQTurn）：滚动发布取消了 ctx 也要记下。
func (s *Server) recordQQFAQCandidate(ctx context.Context, question, answer string) {
	if s.qqFAQCandidates == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	question = qqTruncate(strings.TrimSpace(question), qqFAQCandidateRunes)
	if question == "" {
		return
	}
	subject := strings.Join(strings.Fields(question), "")
	release, err := s.qqReserveDedupe(ctx, "qq-bot-faq-candidate", subject, 7*24*time.Hour)
	if err != nil {
		if !errors.Is(err, account.ErrLimited) {
			slog.Warn("qq bot: faq candidate limit unavailable", "reason", err.Error())
		}
		return
	}
	if _, err := s.qqFAQCandidates.AddQQFAQCandidate(ctx, question, qqTruncate(answer, qqFAQCandidateRunes)); err != nil {
		if qqDatabaseWriteDefinitelyRejected(err) {
			release()
		}
		slog.Warn("qq bot: faq candidate not stored", "reason", err.Error())
	}
}

// qqFAQCandidateCommands 处理「文档缺口」「看 F编号」「写进FAQ F编号」「忽略 F编号」，以及差评的「看 B编号」「写进FAQ B编号」「忽略 B编号」（交给 qqBadAnswerCommand），只有管理员能用；返回处理结果的名字，不是这几个命令时返回空字符串。
// 参数解析使用原文 `text`，列表命令使用已规范化的 `compact`。
func (s *Server) qqFAQCandidateCommands(ctx context.Context, text, compact string, admin bool, reply func(string)) string {
	trimmed := strings.TrimSpace(text)
	action, match := "", []string(nil)
	for _, command := range []struct {
		action  string
		pattern *regexp.Regexp
	}{{"write", qqFAQWriteCommand}, {"ignore", qqFAQIgnoreCommand}, {"view", qqFAQViewCommand}} {
		if match = command.pattern.FindStringSubmatch(trimmed); match != nil {
			action = command.action
			break
		}
	}
	if compact != "文档缺口" && match == nil {
		return ""
	}
	var id int64
	extra := ""
	if match != nil {
		id, _ = strconv.ParseInt(match[2], 10, 64)
		if action == "write" {
			extra = strings.TrimSpace(qqAnswerPrefix.ReplaceAllString(strings.TrimSpace(match[3]), ""))
		}
		if strings.EqualFold(match[1], "b") {
			return s.qqBadAnswerCommand(ctx, action, id, extra, admin, reply)
		}
	}
	if !admin {
		reply("只有管理员能处理文档缺口。")
		return "cmd_faq_candidates"
	}
	if s.qqFAQCandidates == nil {
		reply(qqReplyUnavailable)
		return "cmd_faq_candidates"
	}
	if match == nil {
		s.qqListFAQCandidates(ctx, reply)
		return "cmd_faq_candidates"
	}
	code := "F" + match[2]
	candidate, err := s.qqFAQCandidates.QQFAQCandidate(ctx, id)
	switch {
	case errors.Is(err, account.ErrQQFAQCandidateNotFound):
		reply("没有找到 " + code + "，可能已经处理过或过期了。")
		return "cmd_faq_candidate"
	case err != nil:
		slog.Warn("qq bot: faq candidate unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "cmd_faq_candidate"
	case action == "view":
		reply(s.qqFAQCandidateDetail(candidate, code))
		return "cmd_faq_candidate_view"
	case candidate.Writing && !account.QQFAQCandidateClaimStale(candidate, s.qq.now()):
		reply(code + " 正在写进 FAQ，请稍等一两分钟再看。")
		return "cmd_faq_candidate"
	}
	// 认领超时的候选当作写入被打断，和没处理的一样往下走。
	if action == "ignore" {
		ok, err := s.qqFAQCandidates.CloseQQFAQCandidate(ctx, id, "ignored", "")
		switch {
		case err != nil:
			slog.Warn("qq bot: faq candidate not closed", "reason", err.Error())
			reply(qqReplyUnavailable)
			return "cmd_faq_candidate"
		case !ok:
			reply(code + " 正在写进 FAQ 或已经处理过了。")
			return "cmd_faq_candidate"
		}
		reply("已忽略 " + code + "。")
		return "cmd_faq_candidate_ignore"
	}
	answer := extra
	if answer == "" {
		answer = candidate.Answer
	}
	if answer == "" {
		reply(code + " 还没有答案，请写成「写进FAQ " + code + " 答：……」。")
		return "cmd_faq_candidate"
	}
	claimedAt, claimed, err := s.qqFAQCandidates.ClaimQQFAQCandidateLease(ctx, id)
	switch {
	case err != nil:
		slog.Warn("qq bot: faq candidate not claimed", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "cmd_faq_candidate"
	case !claimed:
		reply(code + " 正在写进 FAQ 或已经处理过了。")
		return "cmd_faq_candidate"
	}
	pull := s.recordQQAnswer(ctx, "问："+candidate.Question+"\n答："+answer, reply)
	// 标记和放回都不跟随处理消息的 ctx：开 PR 失败多半是时限用完了，这时 ctx 已经到期，放不回去候选就要等认领超时才能再处理。
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqFAQCandidateStoreTimeout)
	defer cancel()
	if pull > 0 {
		if closed, err := s.qqFAQCandidates.CloseQQFAQCandidateLease(sctx, id, claimedAt, "done", s.qq.config.KnowledgeRepo+" #"+strconv.FormatInt(pull, 10)); err != nil {
			slog.Warn("qq bot: faq candidate not closed", "reason", err.Error())
		} else if !closed {
			slog.Warn("qq bot: faq candidate claim is stale")
		}
	} else if _, err := s.qqFAQCandidates.ReleaseQQFAQCandidateLease(sctx, id, claimedAt); err != nil {
		slog.Warn("qq bot: faq candidate not released", "reason", err.Error())
	}
	return "cmd_faq_candidate_write"
}

// qqFAQCandidateDetail 是「看 F编号」的回复：完整的问题和机器人当时的回答（各截到 qqFAQCandidateRunes 字）和下一步怎么发。原来列表里答案只有前 60 字，管理员凭这几十个字决定要不要写进官方 FAQ。
func (s *Server) qqFAQCandidateDetail(c account.QQFAQCandidate, code string) string {
	var b strings.Builder
	b.WriteString(code)
	writing := c.Writing && !account.QQFAQCandidateClaimStale(c, s.qq.now())
	if writing {
		b.WriteString("（正在写进 FAQ）")
	} else {
		b.WriteString(qqExpiryNote(c.CreatedAt, account.QQFAQCandidateWindow, s.qq.now()))
	}
	b.WriteString("\n问：" + qqTruncate(c.Question, qqFAQCandidateRunes))
	if c.Answer == "" {
		b.WriteString("\n答：（当时没答上）")
	} else {
		b.WriteString("\n机器人当时的回答：" + qqTruncate(c.Answer, qqFAQCandidateRunes))
	}
	switch {
	case writing:
	case c.Answer == "":
		b.WriteString("\n补上答案写进 FAQ 就 @我 发「写进FAQ " + code + " 答：……」，不要就发「忽略 " + code + "」。")
	default:
		b.WriteString("\n照这个回答写进 FAQ 就 @我 发「写进FAQ " + code + "」，要改答案就在后面写「答：……」，不要就发「忽略 " + code + "」。")
	}
	return b.String()
}

// qqListFAQCandidates 回复文档候选：总数，最早的 qqFAQCandidateList 条，快过期的标上还剩几天。
func (s *Server) qqListFAQCandidates(ctx context.Context, reply func(string)) {
	list, total, err := s.qqFAQCandidates.OpenQQFAQCandidates(ctx, qqFAQCandidateList)
	if err != nil {
		slog.Warn("qq bot: faq candidates unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return
	}
	if len(list) == 0 {
		reply("现在没有待补的文档缺口。")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "答疑里文档没写到的问题共 %d 条", total)
	if total > len(list) {
		fmt.Fprintf(&b, "，先列最早的 %d 条", len(list))
	}
	b.WriteString("（「看 F编号」看全文，「写进FAQ F编号」开 PR，没答上的在后面写「答：……」，「忽略 F编号」拿掉）：")
	now := s.qq.now()
	for _, c := range list {
		fmt.Fprintf(&b, "\n- F%d %s", c.ID, qqTruncate(c.Question, 60))
		if c.Answer == "" {
			b.WriteString("（未答）")
		} else {
			b.WriteString(" → " + qqTruncate(c.Answer, 60))
		}
		b.WriteString(qqExpiryNote(c.CreatedAt, account.QQFAQCandidateWindow, now))
	}
	reply(b.String())
}
