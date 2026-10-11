package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 群友报的 bug 和需求整理成 issue 草稿，管理员一句「提交 D编号」就提上去。原来群友报问题只把一句话概括记进汇总 issue，管理员要自己重新组织、再发「提issue」。现在答疑时模型判断群友在报 bug 或提需求（escalate），机器人照常先给排查办法，同时用管理员提 issue 的同一套整理（qqSummarise）把反馈整理成草稿存进数据库（account.QQDraft，所有副本共享），回复里告诉群友草稿编号；缺平台、版本、复现步骤时一并问出来，群友发「补充 D编号 内容」补上，也可以直接回答追问（「Windows 11，0.9.5」），见 qqSupplementFollowUp。只有管理员能提交，这条规则不变：「草稿」列出待处理的草稿，最早的在前（快过期的先被看到），「看 D编号」看整理结果和原话，「提交 D编号」查重后提交，可以顺带补一句（例如「提交 D12 版本 0.9.5」），误判的草稿「忽略 D编号」拿掉。管理员自己「提issue」缺信息时也存成草稿，补一句「提交 D编号 …」就行，不用把整段描述重打一遍（见 qqAdminDraft）。提交前先在数据库里认领草稿，两个副本不会提两遍；提交失败放回，放回时不受处理消息的时限影响；副本中途被杀掉来不及放回时，认领超过 5 分钟（account.QQDraftClaimTimeout）还没记下 issue 就当作提交被打断，可以再提交，万一真的提了两次，fileQQReport 的查重会把第二次补充到已有的 issue。群友的截图不存进公开 bucket，草稿里只有看图得到的文字描述。

const (
	// qqDraftsDaily 是每个群友每天最多新建的草稿数（auth_rates）。
	qqDraftsDaily = 5
	// qqDraftList 是「草稿」列出的条数。
	qqDraftList = 10
	// qqDraftOriginalRunes 是草稿原话的长度上限，补充太多次时截掉最早的部分以外的内容。
	qqDraftOriginalRunes = 4000
	// qqDraftStoreTimeout 是提交结束后放回认领、记下 issue 编号的时限。这两步不用处理消息的 ctx：提交失败往往正是因为它被取消或超时了（滚动更新、4 分钟时限），再用它连数据库也会失败，草稿就会一直卡在「提交中」。
	qqDraftStoreTimeout = 5 * time.Second
)

// qqDraftStore 是 account.Service 里的草稿，所有副本共享；没有数据库时为 nil。
type qqDraftStore interface {
	CreateQQDraft(ctx context.Context, subject, original, report string) (int64, error)
	QQDraft(ctx context.Context, id int64) (account.QQDraft, error)
	UpdateQQDraft(ctx context.Context, id int64, original, report string) (bool, error)
	ClaimQQDraft(ctx context.Context, id int64) (bool, error)
	ReleaseQQDraft(ctx context.Context, id int64) error
	SetQQDraftIssue(ctx context.Context, id int64, ref string) error
	DismissQQDraft(ctx context.Context, id int64) (bool, error)
	OpenQQDrafts(ctx context.Context, limit int) ([]account.QQDraft, int, error)
}

// qqDraftCommand 匹配「提交 D12 …」「补充 D12 …」「看 D12」「忽略 D12」，分组是命令、编号和后面的内容（「看」「忽略」不用后面的内容）。也认「提交D12」、小写的 d、命令后的冒号（「补充：D1 Windows 11」）、编号前的「#」、d 和数字之间的空格（「补充 d 1 win11」），编号后面可以用句号、分号、顿号隔开内容：原来多一个冒号就认不出，补充的内容被当成提问交给模型。
var qqDraftCommand = regexp.MustCompile(`^(提交|补充|看|忽略)\s*[:：]?\s*[#＃]?[Dd]\s*([0-9]{1,9})(?:[\s:：,，。.;；、]+([\s\S]*))?$`)

func qqDraftCode(id int64) string { return "D" + strconv.FormatInt(id, 10) }

// qqDraftMissing 是草稿还缺的信息：模型的追问，或者 Windows 要区分两个产品时的版本追问。
func (s *Server) qqDraftMissing(ctx context.Context, report qqReport, original string) string {
	if report.Missing != "" {
		return report.Missing
	}
	_, _, question := s.qqTarget(ctx, report, original, false)
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

// qqAdminDraft 在管理员提 issue 缺信息时把反馈存成草稿，返回给管理员的回复；没有草稿存储或存不进去时 ok 为 false，调用方照旧请他补充后重发。原来要把整段描述连同「提issue」重打一遍，现在补一句「提交 D编号 版本 0.9.5」就行。text 是去掉维护者标注后的反馈，标注不存进草稿，示例里带上，照着发不会丢；question 是缺的信息。截图和视频不存进草稿（和群友的草稿一样只留看图得到的文字），提交时不会附上，有附件时说一声。
func (s *Server) qqAdminDraft(ctx context.Context, m qqGroupMessage, text string, report qqReport, notes qqNotes, question string) (string, bool) {
	if s.qqDrafts == nil {
		return "", false
	}
	raw, _ := json.Marshal(report)
	id, err := s.qqDrafts.CreateQQDraft(ctx, m.GroupOpenID+"/"+m.Author.MemberOpenID, qqTruncate(text, qqDraftOriginalRunes), string(raw))
	if err != nil {
		slog.Warn("qq bot: admin draft not stored", "reason", err.Error())
		return "", false
	}
	code := qqDraftCode(id)
	note := "还差：" + qqQuestionEnd(question) + "已存为 " + code + "，补一句就行：\n@水杉助手 提交 " + code + " 版本 0.9.5" + qqNotesText(notes)
	if question == qqWindowsVersionQuestion {
		note += "\n不知道版本就写「版本 未知」。"
	}
	if len(m.Attachments) > 0 {
		note += "\n截图和视频不会跟着草稿提交，需要的话提交后在 issue 里补上。"
	}
	return note, true
}

// qqNotesText 把维护者标注写回「优先级 高」「effort S」的样子，开头带一个空格；没有标注时返回空字符串。
func qqNotesText(notes qqNotes) string {
	text := ""
	if notes.Priority != "" {
		name := qqPriorityNames[notes.Priority]
		if name == "" {
			name = notes.Priority
		}
		text += " 优先级 " + name
	}
	if notes.Effort != "" {
		text += " effort " + notes.Effort
	}
	return text
}

// qqDraftCommands 处理「草稿」「看 D编号」「提交 D编号」「补充 D编号」「忽略 D编号」，返回处理结果的名字；不是这几个命令时返回空字符串。
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
	if match[1] == "忽略" && !admin {
		reply("只有管理员能忽略草稿。")
		return "cmd_draft_dismiss"
	}
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
	case match[1] == "看":
		// 草稿里有群友的原话，和列表一样不给别的群友看；发起人可以看自己的。
		if !admin && !account.QQDraftOwnedBy(draft, m.GroupOpenID+"/"+m.Author.MemberOpenID) {
			reply("只有发起这份草稿的人和管理员能看。")
		} else {
			reply(s.qqDraftDetail(ctx, draft, admin))
		}
		return "cmd_draft_view"
	case draft.Filed && draft.IssueRef == account.QQDraftIgnored:
		reply("草稿 " + qqDraftCode(id) + " 已忽略。")
		return "cmd_draft"
	case draft.Filed && draft.IssueRef != "":
		reply("草稿 " + qqDraftCode(id) + " 已经提交过了：" + draft.IssueRef + "。")
		return "cmd_draft"
	case draft.Filed && !account.QQDraftClaimStale(draft, s.qq.now()):
		// 认领了但还没记下 issue：另一条消息正在提交。原来这里也说「已经提交过了」，提交被打断时 GitHub 上其实什么都没有。
		reply("草稿 " + qqDraftCode(id) + " 正在提交，请稍等一两分钟再看。")
		return "cmd_draft"
	}
	// 认领超时还没记下 issue 的草稿当作提交被打断，和没提交的一样往下走。
	if match[1] == "忽略" {
		ok, err := s.qqDrafts.DismissQQDraft(ctx, id)
		switch {
		case err != nil:
			slog.Warn("qq bot: draft not dismissed", "reason", err.Error())
			reply(qqReplyUnavailable)
		case !ok:
			reply("草稿 " + qqDraftCode(id) + " 已经在提交或提交过了。")
		default:
			reply("已忽略草稿 " + qqDraftCode(id) + "，不会提交。")
		}
		return "cmd_draft_dismiss"
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
		reply(s.qqSupplementedNote(ctx, draft, report, extra))
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

// qqSupplementedNote 是补充成功后的回复：还缺信息时接着问并教群友怎么补，信息齐了就说等管理员提交。还缺信息的写法带着「还想知道」和「补充 D编号」，群友下一句直接回答也能补进草稿（qqDraftFollowUp）。
func (s *Server) qqSupplementedNote(ctx context.Context, draft account.QQDraft, report qqReport, extra string) string {
	code := qqDraftCode(draft.ID)
	if missing := s.qqDraftMissing(ctx, report, draft.Original+"\n补充："+extra); missing != "" {
		return "已补充到 " + code + "。还想知道：" + qqQuestionEnd(missing) + "再 @我 发「补充 " + code + " …」补上就行。"
	}
	return "已补充到 " + code + "，信息齐了，等管理员确认提交。"
}

// qqDraftFollowUpRunes 是群友直接回答追问时算作补充的消息长度上限：更长的多半是在说别的事。
const qqDraftFollowUpRunes = 80

var (
	// qqDraftFollowUp 匹配机器人追问草稿缺的信息的回复，分组是草稿编号：「还想知道：…」后面教群友发的「补充 D编号 …」。起草稿的说明（qqDraftReport）和补充后还缺信息的回复（qqSupplementedNote）都是这个写法，两者都会记进对话历史。
	qqDraftFollowUp = regexp.MustCompile(`还想知道[\s\S]*「补充 D([0-9]{1,9})`)
	// qqDraftTurn 匹配已经起过草稿、补充过草稿或者在已有需求下记过票的一轮回复：这一轮之前群友说的话已经在那份草稿或那个需求里了。
	qqDraftTurn = regexp.MustCompile(`编号 D[0-9]+|已补充到 D[0-9]+|这个需求已经有人提过`)
	// qqDraftAck 匹配去掉 qqBadAnswerFiller 之后只剩客气话或应答的消息：「好的」「谢谢」「收到」「知道了」「OK」。追问之后群友回一句「谢谢」很常见，这不是补充。
	qqDraftAck = regexp.MustCompile(`(?i)^(?:(?:好|行|嗯|哦|噢|ok|okay|谢谢|多谢|感谢|谢|收到|明白|知道|了解|辛苦|麻烦|thx|thanks)了?)+$`)
)

// qqDraftText 是整理草稿用的原话：群友这次和之前几轮说的话，较早的在前，用换行拼起来。只拼群友说的，不拼机器人的回答，免得整理时把排查建议当成群友的描述。从最近一轮起过草稿、补充过草稿或记过票的回复之后拼起：那之前说的已经记在别处，再拼进来，新草稿会混进上一个问题。
func qqDraftText(history []account.QQTurn, question string) string {
	start := 0
	for i, turn := range history {
		if qqDraftTurn.MatchString(turn.Answer) {
			start = i + 1
		}
	}
	parts := make([]string, 0, len(history)-start+1)
	for _, turn := range history[start:] {
		parts = append(parts, turn.Question)
	}
	return strings.Join(append(parts, question), "\n")
}

// qqDraftNoContent 判断消息里有没有能补进草稿的内容：去掉语气字、标点和表情后什么都不剩，或者只剩客气话（qqDraftAck）、只剩一句否定（qqBadAnswerPattern，交给差评重答）时没有。
func qqDraftNoContent(text string) bool {
	stripped := qqBadAnswerFiller.ReplaceAllString(text, "")
	return stripped == "" || qqDraftAck.MatchString(stripped) || qqBadAnswerPattern.MatchString(stripped)
}

// qqSupplementFollowUp 处理机器人追问草稿缺的信息之后群友的直接回答，补进那份草稿，返回处理结果的名字；不算补充时返回空字符串，调用方照常答疑。原来只认「补充 D编号 内容」，群友多半直接回「Windows 11，0.9.5」，被当成新问题去答，模型的历史里也没有草稿编号，常常又判成报问题、再开一份草稿，原来那份一直缺信息。同时满足才算补充：上一轮回复是追问（qqDraftFollowUp，草稿编号从这里取，比扫待处理的草稿更不容易补错）；消息不超过 qqDraftFollowUpRunes 字，不是问句（qqCommandQuestion），也不是只有客气话或否定（qqDraftNoContent）；草稿还在、没提交，而且是这位群友发起的。补充成功时这一轮记进对话历史，还缺信息时下一句照样能接上；补充失败时 qqSupplementDraft 已经回复过，不记这一轮，上一轮的追问还在，群友再说一句仍能补上。
func (s *Server) qqSupplementFollowUp(ctx context.Context, subject string, history []account.QQTurn, text string, reply func(string)) string {
	if s.qqDrafts == nil || s.qqHistory == nil || len(history) == 0 {
		return ""
	}
	match := qqDraftFollowUp.FindStringSubmatch(history[len(history)-1].Answer)
	text = strings.TrimSpace(text)
	if match == nil || utf8.RuneCountInString(text) > qqDraftFollowUpRunes || qqCommandQuestion.MatchString(text) || qqDraftNoContent(text) {
		return ""
	}
	id, _ := strconv.ParseInt(match[1], 10, 64)
	draft, err := s.qqDrafts.QQDraft(ctx, id)
	if err != nil {
		if !errors.Is(err, account.ErrQQDraftNotFound) {
			slog.Warn("qq bot: draft unavailable", "reason", err.Error())
		}
		return ""
	}
	if !account.QQDraftOwnedBy(draft, subject) || (draft.Filed && !account.QQDraftClaimStale(draft, s.qq.now())) {
		return ""
	}
	report, ok := s.qqSupplementDraft(ctx, draft, text, false, reply)
	if !ok {
		return "cmd_draft_supplement_followup"
	}
	note := s.qqSupplementedNote(ctx, draft, report, text)
	reply(note)
	if err := s.qqHistory.RecordQQTurn(ctx, subject, text, note); err != nil {
		slog.Warn("qq bot: conversation not recorded", "reason", err.Error())
	}
	return "cmd_draft_supplement_followup"
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
	// 管理员写了「版本 未知」时不再追问版本，正文写明版本未知（qqMarkVersionUnknown）；只看管理员这次补的内容，群友补充里的「版本 未知」不算。
	unknown := qqVersionUnknown.MatchString(text)
	if unknown {
		report = qqMarkVersionUnknown(report)
	}
	repo, label, question := s.qqTarget(ctx, report, original, unknown)
	if question != "" {
		message := "草稿 " + code + " 还差：" + qqQuestionEnd(question) + "可以 @我 发「提交 " + code + " 版本 0.9.5」把缺的信息带上一起提交。"
		if question == qqWindowsVersionQuestion {
			message += "不知道版本就写「版本 未知」。"
		}
		reply(message)
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
	number, duplicate, err := s.fileQQReport(ctx, repo, label, report, nil, 0, notes)
	if err != nil {
		slog.Warn("qq bot: draft not filed", "repo", repo, "reason", err.Error())
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqDraftStoreTimeout)
		if err := s.qqDrafts.ReleaseQQDraft(rctx, draft.ID); err != nil {
			slog.Warn("qq bot: draft not released", "reason", err.Error())
		}
		cancel()
		reply(qqReplyUnavailable)
		return "failed"
	}
	_, short, _ := strings.Cut(repo, "/")
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqDraftStoreTimeout)
	if err := s.qqDrafts.SetQQDraftIssue(rctx, draft.ID, short+" #"+strconv.FormatInt(number, 10)); err != nil {
		slog.Warn("qq bot: draft issue not recorded", "reason", err.Error())
	}
	cancel()
	if duplicate {
		reply("草稿 " + code + " 和已有的问题重复，已补充到 " + qqIssueNumberText(repo, number) + "。" + s.qqProgressHint(repo, number))
		return "filed_duplicate"
	}
	reply("草稿 " + code + " 已提交为 " + qqIssueNumberText(repo, number) + "。" + s.qqProgressHint(repo, number) + qqReplyPublic)
	return "filed"
}

// qqListDrafts 回复待处理的草稿：总数，最早的 qqDraftList 份的编号、标题、还缺的信息，快过期的标上还剩几天。
func (s *Server) qqListDrafts(ctx context.Context, reply func(string)) {
	if s.qqDrafts == nil {
		reply(qqReplyUnavailable)
		return
	}
	drafts, total, err := s.qqDrafts.OpenQQDrafts(ctx, qqDraftList)
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
	fmt.Fprintf(&b, "待处理的草稿共 %d 份", total)
	if total > len(drafts) {
		fmt.Fprintf(&b, "，先列最早的 %d 份", len(drafts))
	}
	b.WriteString("（「看 D编号」看全文，「提交 D编号」提交，「忽略 D编号」不提交）：")
	now := s.qq.now()
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
		b.WriteString(qqExpiryNote(d.CreatedAt, account.QQDraftWindow, now))
	}
	reply(b.String())
}

// qqExpiryDays 是列表里开始标「n 天后过期」的剩余天数。
const qqExpiryDays = 3

// qqExpiryNote 是列表里快过期的条目后面的「（n 天后过期）」：有效期剩下不到 qqExpiryDays 天时才标，不足一天按一天算；已经过了有效期（等着被 Store.Prune 删掉）或者还早时返回空字符串。原来过期的条目悄悄消失，管理员不知道哪些该先处理。
func qqExpiryNote(created time.Time, window time.Duration, now time.Time) string {
	left := created.Add(window).Sub(now)
	if left <= 0 || left >= qqExpiryDays*24*time.Hour {
		return ""
	}
	return fmt.Sprintf("（%d 天后过期）", int(math.Ceil(left.Hours()/24)))
}

// qqDraftDetail 是「看 D编号」的回复：状态、整理结果的各项和群友的原话（截到 300 字），管理员先看清楚再决定提交还是忽略。原来列表只有一句标题，管理员看不到现象、步骤和平台就得决定要不要在 GitHub 上公开提 issue。
func (s *Server) qqDraftDetail(ctx context.Context, draft account.QQDraft, admin bool) string {
	code := qqDraftCode(draft.ID)
	var report qqReport
	_ = json.Unmarshal([]byte(draft.Report), &report)
	kind := "bug"
	if report.Kind == "feature" {
		kind = "需求"
	}
	pending := false
	status := "待提交"
	switch {
	case draft.Filed && draft.IssueRef == account.QQDraftIgnored:
		status = "已忽略"
	case draft.Filed && draft.IssueRef != "":
		status = "已提交为 " + draft.IssueRef
	case draft.Filed && !account.QQDraftClaimStale(draft, s.qq.now()):
		status = "正在提交"
	default:
		pending = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "草稿 %s（%s，%s，%s）", code, kind, draft.CreatedAt.In(qqStatsZone).Format("01-02 15:04"), status)
	platform := qqPlatformNames[report.Platform]
	if platform == "" {
		platform = report.Platform
	}
	for _, field := range [][2]string{{"标题", report.Title}, {"平台", platform}, {"输入法版本", report.IMEVersion}, {"现象", report.Problem}, {"复现步骤", report.Steps}, {"期望", report.Expected}, {"使用场景", report.Scenario}, {"建议", report.Proposal}} {
		if strings.TrimSpace(field[1]) != "" {
			b.WriteString("\n" + field[0] + "：" + field[1])
		}
	}
	b.WriteString("\n原话：" + qqTruncate(draft.Original, 300))
	if pending {
		if missing := s.qqDraftMissing(ctx, report, draft.Original); missing != "" {
			b.WriteString("\n还缺：" + missing)
		}
		if admin {
			b.WriteString("\n要提交就 @我 发「提交 " + code + "」，不提交就发「忽略 " + code + "」。")
		}
	}
	return b.String()
}
