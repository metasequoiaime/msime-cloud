package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 群友报的 bug 和需求整理成 issue 草稿，管理员一句「提交 D编号」就提上去。原来群友报问题只把一句话概括记进汇总 issue，管理员要自己重新组织、再发「提issue」。现在答疑时模型判断群友在报 bug 或提需求（escalate），机器人照常先给排查办法，同时用管理员提 issue 的同一套整理（qqSummarise）把反馈整理成草稿存进数据库（account.QQDraft，所有副本共享），回复里告诉群友草稿编号；缺平台、版本、复现步骤时一并问出来，群友发「补充 D编号 内容」补上。只有管理员能提交，这条规则不变：「草稿」列出待处理的草稿，「提交 D编号」查重后提交，可以顺带补一句（例如「提交 D12 版本 0.9.5」）。提交前先在数据库里认领草稿，两个副本不会提两遍；提交失败放回。群友的截图不存进公开 bucket，草稿里只有看图得到的文字描述。

const (
	// qqDraftsDaily 是每个群友每天最多新建的草稿数（auth_rates）。
	qqDraftsDaily = 5
	// qqDraftList 是「草稿」列出的条数。
	qqDraftList = 10
	// qqDraftOriginalRunes 是草稿原话的长度上限，补充太多次时截掉最早的部分以外的内容。
	qqDraftOriginalRunes = 4000
)

// qqDraftStore 是 account.Service 里的草稿，所有副本共享；没有数据库时为 nil。
type qqDraftStore interface {
	CreateQQDraft(ctx context.Context, subject, original, report string) (int64, error)
	QQDraft(ctx context.Context, id int64) (account.QQDraft, error)
	UpdateQQDraft(ctx context.Context, id int64, original, report string) (bool, error)
	ClaimQQDraft(ctx context.Context, id int64) (bool, error)
	ReleaseQQDraft(ctx context.Context, id int64) error
	SetQQDraftIssue(ctx context.Context, id int64, ref string) error
	OpenQQDrafts(ctx context.Context, limit int) ([]account.QQDraft, error)
}

// qqDraftCommand 匹配「提交 D12 …」「补充 D12 …」，分组是命令、编号和后面的内容。也认「提交D12」、小写的 d、命令后的冒号（「补充：D1 Windows 11」）、编号前的「#」、d 和数字之间的空格（「补充 d 1 win11」），编号后面可以用句号、分号、顿号隔开内容：原来多一个冒号就认不出，补充的内容被当成提问交给模型。
var qqDraftCommand = regexp.MustCompile(`^(提交|补充)\s*[:：]?\s*[#＃]?[Dd]\s*([0-9]{1,9})(?:[\s:：,，。.;；、]+([\s\S]*))?$`)

func qqDraftCode(id int64) string { return "D" + strconv.FormatInt(id, 10) }

// qqDraftMissing 是草稿还缺的信息：模型的追问，或者 Windows 要区分两个产品时的版本追问。
func (s *Server) qqDraftMissing(ctx context.Context, report qqReport, original string) string {
	if report.Missing != "" {
		return report.Missing
	}
	_, _, question := s.qqTarget(ctx, report, original)
	return question
}

// qqDraftReport 把群友报的问题整理成草稿，返回答案发出后单独回复的说明；不是具体的 bug 或需求、额度用完或者出错时返回空字符串，调用方照旧记进汇总 issue。
func (s *Server) qqDraftReport(ctx context.Context, subject, text string) string {
	if s.qqDrafts == nil {
		return ""
	}
	switch err := s.limits.RateLimit(ctx, "qq-bot-drafts", subject, qqDraftsDaily, 24*time.Hour); {
	case errors.Is(err, account.ErrLimited):
		return ""
	case err != nil:
		slog.Warn("qq bot: draft limit unavailable", "reason", err.Error())
		return ""
	}
	report, err := s.qqSummarise(ctx, text)
	if err != nil || report.Kind == "other" {
		if err != nil {
			slog.Warn("qq bot: draft not summarised", "reason", err.Error())
		}
		return ""
	}
	// 需求先找有没有人提过，提过就在那里记一票，不起新草稿（见 qq_votes.go）。
	if report.Kind == "feature" {
		if note := s.qqVoteFeature(ctx, subject, report, text); note != "" {
			return note
		}
	}
	raw, _ := json.Marshal(report)
	id, err := s.qqDrafts.CreateQQDraft(ctx, subject, qqTruncate(text, qqDraftOriginalRunes), string(raw))
	if err != nil {
		slog.Warn("qq bot: draft not stored", "reason", err.Error())
		return ""
	}
	code := qqDraftCode(id)
	// 群友看到的说法不用「草稿」「GitHub」这类内部叫法；编号要留着，补充时用得到。
	note := "我已经把这个问题记下来了，编号 " + code + "，管理员确认后会转给开发者。"
	if missing := s.qqDraftMissing(ctx, report, text); missing != "" {
		note += "还想知道：" + qqQuestionEnd(missing) + "补上后 @我 发「补充 " + code + " Windows 11，版本 0.9.5」这样就行。"
	}
	return note
}

// qqDraftCommands 处理「草稿」「提交 D编号」「补充 D编号」，返回处理结果的名字；不是这几个命令时返回空字符串。
func (s *Server) qqDraftCommands(ctx context.Context, m qqGroupMessage, text string, admin bool, reply func(string)) string {
	compact := qqCompact(text)
	if compact == "草稿" || compact == "草稿列表" {
		if !admin {
			reply("只有管理员能看草稿列表。")
			return "cmd_drafts"
		}
		s.qqListDrafts(ctx, reply)
		return "cmd_drafts"
	}
	match := qqDraftCommand.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return ""
	}
	id, _ := strconv.ParseInt(match[2], 10, 64)
	extra := strings.TrimSpace(match[3])
	if s.qqDrafts == nil {
		reply(qqReplyUnavailable)
		return "cmd_draft"
	}
	draft, err := s.qqDrafts.QQDraft(ctx, id)
	switch {
	case errors.Is(err, account.ErrQQDraftNotFound):
		reply("没有找到草稿 " + qqDraftCode(id) + "，可能已经过期了。")
		return "cmd_draft"
	case err != nil:
		slog.Warn("qq bot: draft unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "cmd_draft"
	case draft.Filed:
		done := " 已经提交过了"
		if draft.IssueRef != "" {
			done += "：" + draft.IssueRef
		}
		reply("草稿 " + qqDraftCode(id) + done + "。")
		return "cmd_draft"
	}
	if match[1] == "补充" {
		if !admin && !account.QQDraftOwnedBy(draft, m.GroupOpenID+"/"+m.Author.MemberOpenID) {
			reply("只有发起这份草稿的人和管理员能补充。")
			return "cmd_draft_supplement"
		}
		if extra == "" {
			reply("请在编号后面写要补充的内容，例如 @我 发「补充 " + qqDraftCode(id) + " Windows 11，版本 0.9.5」。")
			return "cmd_draft_supplement"
		}
		report, ok := s.qqSupplementDraft(ctx, draft, extra, false, reply)
		if !ok {
			return "cmd_draft_supplement"
		}
		code := qqDraftCode(id)
		if missing := s.qqDraftMissing(ctx, report, draft.Original+"\n补充："+extra); missing != "" {
			reply("已补充到 " + code + "。还想知道：" + qqQuestionEnd(missing) + "再 @我 发「补充 " + code + " …」补上就行。")
		} else {
			reply("已补充到 " + code + "，信息齐了，等管理员确认提交。")
		}
		return "cmd_draft_supplement"
	}
	if !admin {
		reply("只有管理员能提交草稿。草稿 " + qqDraftCode(id) + " 已经记下了，管理员看到后会处理。")
		return "cmd_draft_file"
	}
	return "cmd_draft_file_" + s.qqFileDraft(ctx, draft, extra, reply)
}

// qqSupplementDraft 把补充内容并进草稿原话，重新整理并保存。filing 表示管理员在「提交 D编号 …」时顺带补充。几种失败分开回复：补充后整理成 other 不是临时故障，重发同一句结果不变，所以请群友写具体些、草稿不改；整理或数据库出错才是「稍后再试」；草稿已经提交或过期才说改不了。
func (s *Server) qqSupplementDraft(ctx context.Context, draft account.QQDraft, extra string, filing bool, reply func(string)) (qqReport, bool) {
	code := qqDraftCode(draft.ID)
	original := qqTruncate(draft.Original+"\n补充："+extra, qqDraftOriginalRunes)
	report, err := s.qqSummarise(ctx, original)
	switch {
	case err != nil:
		slog.Warn("qq bot: draft not summarised", "reason", err.Error())
		reply(qqReplyUnavailable)
		return report, false
	case report.Kind == "other":
		message := "这条补充我没看懂，能说具体点吗？比如 @我 发「补充 " + code + " Windows 11，版本 0.9.5，在微信里打字时候选框不见了」。"
		if filing {
			message += "也可以直接 @我 发「提交 " + code + "」。"
		}
		reply(message)
		return report, false
	}
	raw, _ := json.Marshal(report)
	ok, err := s.qqDrafts.UpdateQQDraft(ctx, draft.ID, original, string(raw))
	switch {
	case err != nil:
		slog.Warn("qq bot: draft not updated", "reason", err.Error())
		reply(qqReplyUnavailable)
		return report, false
	case !ok:
		reply("草稿 " + code + " 已经提交或过期了，改不了。")
		return report, false
	}
	return report, true
}

// qqQuestionEnd 给模型写的追问补上句末的问号，后面好直接接下一句；已经以标点结尾时原样返回。
func qqQuestionEnd(question string) string {
	if strings.HasSuffix(question, "？") || strings.HasSuffix(question, "?") || strings.HasSuffix(question, "。") || strings.HasSuffix(question, "！") || strings.HasSuffix(question, "!") {
		return question
	}
	return question + "？"
}

// qqFileDraft 提交一份草稿，返回结果的名字。extra 是管理员顺带补的内容。
func (s *Server) qqFileDraft(ctx context.Context, draft account.QQDraft, extra string, reply func(string)) string {
	code := qqDraftCode(draft.ID)
	var report qqReport
	if json.Unmarshal([]byte(draft.Report), &report) != nil {
		reply(qqReplyUnavailable)
		return "failed"
	}
	original := draft.Original
	text, notes := qqParseNotes(extra)
	if text != "" {
		// 管理员顺带补了内容（例如版本号）时先并进草稿再提交。
		updated, ok := s.qqSupplementDraft(ctx, draft, text, true, reply)
		if !ok {
			return "failed"
		}
		report, original = updated, qqTruncate(draft.Original+"\n补充："+text, qqDraftOriginalRunes)
	}
	repo, label, question := s.qqTarget(ctx, report, original)
	if question != "" {
		reply("草稿 " + code + " 还差：" + qqQuestionEnd(question) + "可以 @我 发「提交 " + code + " 版本 0.9.5」把缺的信息带上一起提交。")
		return "needs_info"
	}
	claimed, err := s.qqDrafts.ClaimQQDraft(ctx, draft.ID)
	switch {
	case err != nil:
		slog.Warn("qq bot: draft not claimed", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "failed"
	case !claimed:
		reply("草稿 " + code + " 已经在提交或提交过了。")
		return "duplicate"
	}
	reply("收到，正在提交草稿 " + code + "。")
	number, duplicate, err := s.fileQQReport(ctx, repo, label, report, "", notes)
	if err != nil {
		slog.Warn("qq bot: draft not filed", "repo", repo, "reason", err.Error())
		if err := s.qqDrafts.ReleaseQQDraft(ctx, draft.ID); err != nil {
			slog.Warn("qq bot: draft not released", "reason", err.Error())
		}
		reply(qqReplyUnavailable)
		return "failed"
	}
	_, short, _ := strings.Cut(repo, "/")
	if err := s.qqDrafts.SetQQDraftIssue(ctx, draft.ID, short+" #"+strconv.FormatInt(number, 10)); err != nil {
		slog.Warn("qq bot: draft issue not recorded", "reason", err.Error())
	}
	if duplicate {
		reply("草稿 " + code + " 和已有的问题重复，已补充到 " + qqIssueNumberText(repo, number) + "。" + s.qqProgressHint(repo, number))
		return "filed_duplicate"
	}
	reply("草稿 " + code + " 已提交为 " + qqIssueNumberText(repo, number) + "。" + s.qqProgressHint(repo, number) + qqReplyPublic)
	return "filed"
}

// qqListDrafts 回复待处理的草稿：编号、标题和还缺的信息。
func (s *Server) qqListDrafts(ctx context.Context, reply func(string)) {
	if s.qqDrafts == nil {
		reply(qqReplyUnavailable)
		return
	}
	drafts, err := s.qqDrafts.OpenQQDrafts(ctx, qqDraftList)
	if err != nil {
		slog.Warn("qq bot: drafts unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return
	}
	if len(drafts) == 0 {
		reply("现在没有待处理的草稿。")
		return
	}
	var b strings.Builder
	b.WriteString("待处理的草稿（发「提交 D编号」提交）：")
	for _, d := range drafts {
		var report qqReport
		_ = json.Unmarshal([]byte(d.Report), &report)
		kind := "bug"
		if report.Kind == "feature" {
			kind = "需求"
		}
		fmt.Fprintf(&b, "\n- %s（%s，%s）%s", qqDraftCode(d.ID), kind, d.CreatedAt.In(qqStatsZone).Format("01-02 15:04"), report.Title)
		if missing := s.qqDraftMissing(ctx, report, d.Original); missing != "" {
			b.WriteString("（缺：" + missing + "）")
		}
	}
	reply(b.String())
}
