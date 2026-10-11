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

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 把答案沉淀进文档。机器人现查 GitHub 才答上的问题，说明文档里缺这块；答不上的问题更是。这些都记成「文档候选」（account.QQFAQCandidate，编号 F12 这样），管理员发「文档缺口」看列表，「写进FAQ F12」用「记录答案」的流程（qq_faq.go）整理成 FAQ 的一条给知识库开 PR，没答上的在后面补上答案：「写进FAQ F12 答：……」；「忽略 F12」拿掉。同一个问题 7 天内只记一次（auth_rates）。

const (
	// qqFAQCandidateList 是「文档缺口」列出的条数，qqFAQCandidateRunes 是候选里问题和答案的长度上限。
	qqFAQCandidateList  = 10
	qqFAQCandidateRunes = 600
)

// qqFAQCandidateStore 是 account.Service 里的文档候选，所有副本共享；没有数据库时为 nil。
type qqFAQCandidateStore interface {
	AddQQFAQCandidate(ctx context.Context, question, answer string) (int64, error)
	QQFAQCandidate(ctx context.Context, id int64) (account.QQFAQCandidate, error)
	OpenQQFAQCandidates(ctx context.Context, limit int) ([]account.QQFAQCandidate, error)
	CloseQQFAQCandidate(ctx context.Context, id int64, state, ref string) (bool, error)
}

// qqFAQWriteCommand 匹配「写进FAQ F12 …」，分组是编号和后面的内容；qqFAQIgnoreCommand 匹配「忽略 F12」，分组是编号。动词也认「写入」「加进」「加到」「放进」「记进」，也认命令后的冒号、编号前的「#」和 f 与数字之间的空格：原来管理员写成「写进FAQ：F3」「写入FAQ F3」就被当成反馈去整理，F3 没写进 FAQ。
var (
	qqFAQWriteCommand  = regexp.MustCompile(`(?i)^(?:写进|写入|加进|加到|放进|记进)\s*faq\s*[:：]?\s*[#＃]?f\s*([0-9]{1,9})(?:[\s:：,，。.;；、]+([\s\S]*))?$`)
	qqFAQIgnoreCommand = regexp.MustCompile(`(?i)^忽略\s*[:：]?\s*[#＃]?f\s*([0-9]{1,9})(?:[\s:：,，。.;；、]+[\s\S]*)?$`)
)

// qqAnswerPrefix 是「写进FAQ F12 答：……」里答案的开头。
var qqAnswerPrefix = regexp.MustCompile(`^答[:：]\s*`)

// recordQQFAQCandidate 记下一条文档候选；同一个问题 7 天内只记一次，出错只记日志。
func (s *Server) recordQQFAQCandidate(ctx context.Context, question, answer string) {
	if s.qqFAQCandidates == nil {
		return
	}
	question = qqTruncate(strings.TrimSpace(question), qqFAQCandidateRunes)
	if question == "" {
		return
	}
	if err := s.limits.RateLimit(ctx, "qq-bot-faq-candidate", strings.Join(strings.Fields(question), ""), 1, 7*24*time.Hour); err != nil {
		if !errors.Is(err, account.ErrLimited) {
			slog.Warn("qq bot: faq candidate limit unavailable", "reason", err.Error())
		}
		return
	}
	if _, err := s.qqFAQCandidates.AddQQFAQCandidate(ctx, question, qqTruncate(answer, qqFAQCandidateRunes)); err != nil {
		slog.Warn("qq bot: faq candidate not stored", "reason", err.Error())
	}
}

// qqFAQCandidateCommands 处理「文档缺口」「写进FAQ F编号」「忽略 F编号」，只有管理员能用；返回处理结果的名字，不是这几个命令时返回空字符串。
func (s *Server) qqFAQCandidateCommands(ctx context.Context, text string, admin bool, reply func(string)) string {
	write := qqFAQWriteCommand.FindStringSubmatch(strings.TrimSpace(text))
	ignore := qqFAQIgnoreCommand.FindStringSubmatch(strings.TrimSpace(text))
	if qqCompact(text) != "文档缺口" && write == nil && ignore == nil {
		return ""
	}
	if !admin {
		reply("只有管理员能处理文档缺口。")
		return "cmd_faq_candidates"
	}
	if s.qqFAQCandidates == nil {
		reply(qqReplyUnavailable)
		return "cmd_faq_candidates"
	}
	if write == nil && ignore == nil {
		s.qqListFAQCandidates(ctx, reply)
		return "cmd_faq_candidates"
	}
	match := write
	if match == nil {
		match = ignore
	}
	number := match[1]
	id, _ := strconv.ParseInt(number, 10, 64)
	code := "F" + number
	candidate, err := s.qqFAQCandidates.QQFAQCandidate(ctx, id)
	switch {
	case errors.Is(err, account.ErrQQFAQCandidateNotFound):
		reply("没有找到 " + code + "，可能已经处理过或过期了。")
		return "cmd_faq_candidate"
	case err != nil:
		slog.Warn("qq bot: faq candidate unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "cmd_faq_candidate"
	}
	if ignore != nil {
		if _, err := s.qqFAQCandidates.CloseQQFAQCandidate(ctx, id, "ignored", ""); err != nil {
			slog.Warn("qq bot: faq candidate not closed", "reason", err.Error())
			reply(qqReplyUnavailable)
			return "cmd_faq_candidate"
		}
		reply("已忽略 " + code + "。")
		return "cmd_faq_candidate_ignore"
	}
	answer := strings.TrimSpace(qqAnswerPrefix.ReplaceAllString(strings.TrimSpace(write[2]), ""))
	if answer == "" {
		answer = candidate.Answer
	}
	if answer == "" {
		reply(code + " 还没有答案，请写成「写进FAQ " + code + " 答：……」。")
		return "cmd_faq_candidate"
	}
	pull := s.recordQQAnswer(ctx, "问："+candidate.Question+"\n答："+answer, reply)
	if pull > 0 {
		if _, err := s.qqFAQCandidates.CloseQQFAQCandidate(ctx, id, "done", s.qq.config.KnowledgeRepo+" #"+strconv.FormatInt(pull, 10)); err != nil {
			slog.Warn("qq bot: faq candidate not closed", "reason", err.Error())
		}
	}
	return "cmd_faq_candidate_write"
}

// qqListFAQCandidates 回复最近的文档候选。
func (s *Server) qqListFAQCandidates(ctx context.Context, reply func(string)) {
	list, err := s.qqFAQCandidates.OpenQQFAQCandidates(ctx, qqFAQCandidateList)
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
	b.WriteString("最近答疑里文档没写到的问题（「写进FAQ F编号」开 PR，没答上的在后面写「答：……」，「忽略 F编号」拿掉）：")
	for _, c := range list {
		fmt.Fprintf(&b, "\n- F%d %s", c.ID, qqTruncate(c.Question, 60))
		if c.Answer == "" {
			b.WriteString("（未答）")
		} else {
			b.WriteString(" → " + qqTruncate(c.Answer, 60))
		}
	}
	reply(b.String())
}
