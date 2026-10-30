package server

import (
	"context"
	"strings"
)

// 群答疑机器人的平台无关部分。QQ 与 Telegram 群消息都先转成 botMessage，交给同一个 handleBotMessage 处理：命令、额度、过渡语、看截图、管理员提 issue 和知识库答疑都只写一份。平台之间的差别收在 botChannel 里：怎么回复（QQ 要带 msg_seq、最多 5 次、拒收未报备的链接，Telegram 引用原消息即可）和谁算管理员。

// botChannel 是 handleBotMessage 和一个聊天平台之间的接口。
type botChannel interface {
	// reply 回复 m；seq 是这条消息的第几次回复，从 1 起。错误只带状态和数字错误码，不含凭据或上游描述。
	reply(ctx context.Context, m botMessage, seq int, text string) error
	// maxReplies 是同一条消息最多回复几次，0 表示不限。
	maxReplies() int
	// rejectsLinks 表示平台会拒收带链接的回复，这时去掉链接重发一次。
	rejectsLinks() bool
	// isAdmin 判断 m 的发送者是不是管理员：管理员能提 issue、处理草稿和文档缺口、看周报，额度也单独计算。
	isAdmin(ctx context.Context, m botMessage) bool
}

// botPrivateSender 是能私聊消息发送者的平台（Telegram）。群里的绑定码这类只该本人看到的内容，经它私聊发送；QQ 官方机器人不能主动私聊群友，不实现它。
type botPrivateSender interface {
	// inGroup 表示消息来自群聊；私聊里的回复本来就只有发送者看得到。
	inGroup() bool
	// sendPrivate 私聊发送者；对方没有私聊过机器人（Telegram 上 Bot 不能先开口）时返回错误。
	sendPrivate(ctx context.Context, text string) error
}

const (
	botPlatformQQ       = "qq"
	botPlatformTelegram = "telegram"
)

// botMessage 是群答疑机器人处理的一条消息。Group 和 Member 是平台内的群和成员标识，Telegram 的写成 tg:<数字 ID>，和 QQ 的 openid 不会重合；对话历史、草稿归属、额度和排队都按「Group/Member」区分。
type botMessage struct {
	// ID 是消息 ID：记差评、去重时用。
	ID string
	// Text 是去掉 @ 机器人之后的文字。
	Text          string
	Group, Member string
	// Role 是消息事件里带的群身份（QQ 的 owner、admin、member），可能为空。
	Role        string
	Attachments []qqAttachment
	// Quote 是被引用（Telegram 里是被回复）的那条消息。
	Quote qqQuote
	// Platform 是 botPlatformQQ 或 botPlatformTelegram，写进处理结果的日志；只有 QQ 有的功能（绑定 QQ 查打字数据）在别的平台上不提供。测试里直接调命令时为空，按 QQ 处理。
	Platform string
	// Automatic 表示没有明确呼叫机器人，须先识别提问，且不能执行命令或自动提交反馈。
	Automatic bool
	channel   botChannel
}

// subject 是这位成员在这个群里的标识：「Group/Member」。
func (m botMessage) subject() string { return m.Group + "/" + m.Member }

func (m botMessage) onTelegram() bool { return m.Platform == botPlatformTelegram }

// botOriginTelegram 是 qqReport.Origin 里表示反馈来自 Telegram 群的值；为空时来自 QQ 群，包括加这个字段之前存下的草稿。
const botOriginTelegram = "telegram"

// botSubjectOrigin 按 subject（「Group/Member」）判断反馈来自哪个平台，返回 qqReport.Origin 的值。
func botSubjectOrigin(subject string) string {
	if strings.HasPrefix(subject, "tg:") {
		return botOriginTelegram
	}
	return ""
}

// botOriginGroup 是来源在公开文字（issue 页脚、补充评论、词条投稿备注）里的写法：「QQ 群」或「Telegram 群」。
func botOriginGroup(origin string) string {
	if origin == botOriginTelegram {
		return "Telegram 群"
	}
	return "QQ 群"
}

// qqChannel 是 QQ 开放平台：被动回复要带递增的 msg_seq，同一条消息最多回复 qqBotMaxReplies 次，回复里只允许报备过的域名；管理员见 qqIsAdmin。
type qqChannel struct{ s *Server }

func (c qqChannel) reply(ctx context.Context, m botMessage, seq int, text string) error {
	return c.s.qq.reply(ctx, m.Group, m.ID, seq, text)
}

func (qqChannel) maxReplies() int    { return qqBotMaxReplies }
func (qqChannel) rejectsLinks() bool { return true }

func (c qqChannel) isAdmin(ctx context.Context, m botMessage) bool {
	return c.s.qqIsAdmin(ctx, m.Group, m.Member, m.Role)
}

// qqBotMessage 把 QQ 群消息转成 botMessage：去掉 @ 标记和表情标签（qqFeedbackText），引用已经由 acceptQQMessage 解析好。
func (s *Server) qqBotMessage(m qqGroupMessage) botMessage {
	return botMessage{
		ID: m.ID, Text: qqFeedbackText(m.Content), Group: m.GroupOpenID, Member: m.Author.MemberOpenID, Role: m.Author.MemberRole,
		Attachments: m.Attachments, Quote: m.Quote, Platform: botPlatformQQ, Automatic: m.Automatic, channel: qqChannel{s},
	}
}
