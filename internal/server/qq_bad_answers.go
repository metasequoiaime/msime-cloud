package server

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 答错了能被记下来。原来机器人答错了没人知道，只能靠管理员在群里看到。现在群友在追问的时间范围里回一句「不对」「答错了」「没用」这类短话，机器人把上一轮的问答连同这句反馈记下来（account.QQBadAnswer），强制现查、用强模型把上一个问题重答一次；管理员发「差评」看最近被说答错的问题，用来改提示词和补文档。

const (
	// qqBadAnswerRunes 是算作「说上一个回答不对」的消息长度上限：长消息多半是新问题。
	qqBadAnswerRunes = 30
	// qqBadAnswerList、qqBadAnswerWindow 是「差评」列出的条数和时间范围。
	qqBadAnswerList   = 10
	qqBadAnswerWindow = 14 * 24 * time.Hour
	// qqRetryPrefix 是重答的开头。
	qqRetryPrefix = "我重新查了一下："
)

// qqBadAnswerWords 是说上一个回答不对的说法。原来在消息里任意位置出现就算差评，30 分钟内的新问题「删除键没用了」「语音识别不准怎么办」会被当成差评，转去把上一个问题重答一遍，新问题没人理。
const qqBadAnswerWords = `不对|答错|错了|没用|不准|不是这样|胡说|瞎说|乱说|不行|没解决|答非所问|看不懂`

var (
	// qqBadAnswerPattern 匹配去掉 qqBadAnswerFiller 之后只剩一句否定的消息：「不对」「不对啊」「答错了」「好像还是不行」。
	qqBadAnswerPattern = regexp.MustCompile(`^(?:` + qqBadAnswerWords + `)了?$`)
	// qqBadAnswerLead 匹配以否定开头、隔一个标点或空格再接纠正内容的消息：「不对，我是安卓」。
	qqBadAnswerLead = regexp.MustCompile(`^(?:` + qqBadAnswerWords + `)了?[，,。：: ]`)
	// qqBadAnswerFiller 是判断前去掉的语气字、标点、表情和「还是」「好像」「感觉」「你的回答」这类垫话。不去掉单独的「答」和「了」，免得「答错了」「答非所问」被削成别的词。
	qqBadAnswerFiller = regexp.MustCompile(`还是|好像|感觉|你说的|你说得|你这个|你的|这个|回答|答案|还|你|的|[啊呀哦嘛吧呢啦哈哇喔噢]|[\p{P}\p{S}\s]`)
	// qqBadAnswerQuestion 匹配提问的说法：带这些的消息是在问新问题，一律不算差评（「候选词选错了怎么撤回」）。
	qqBadAnswerQuestion = regexp.MustCompile(`怎么|如何|为什么|为啥|吗|？|\?`)
)

// qqBadAnswerStore 是 account.Service 里被说答错的回答；没有数据库时为 nil。
type qqBadAnswerStore interface {
	AddQQBadAnswer(ctx context.Context, question, answer, feedback, retry string) error
	RecentQQBadAnswers(ctx context.Context, since time.Duration, limit int) ([]account.QQBadAnswer, error)
}

// qqIsBadAnswerFeedback 判断这条消息是不是在说上一个回答不对：有上一轮、上一轮答了（不是答不上），消息很短、不是提问，并且整句只是一句否定（qqBadAnswerPattern），或者以否定开头再接纠正内容（qqBadAnswerLead）。
func qqIsBadAnswerFeedback(history []account.QQTurn, text string) bool {
	if len(history) == 0 || history[len(history)-1].Answer == qqUnansweredTurn {
		return false
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > qqBadAnswerRunes || qqBadAnswerQuestion.MatchString(text) {
		return false
	}
	return qqBadAnswerPattern.MatchString(qqBadAnswerFiller.ReplaceAllString(text, "")) || qqBadAnswerLead.MatchString(text)
}

// qqRetryQuestion 是重答时交给模型的问题：上一个问题、上一个回答和群友的反馈。
func qqRetryQuestion(last account.QQTurn, feedback string) string {
	return last.Question + "\n\n（群友说上一个回答不对：「" + feedback + "」。上一个回答是：" + last.Answer + "。请重新查资料，换个思路认真回答，不要重复上一个回答里的错误。）"
}

// recordQQBadAnswer 记下被说答错的一轮，出错只记日志。
func (s *Server) recordQQBadAnswer(ctx context.Context, last account.QQTurn, feedback, retry string) {
	if s.qqBadAnswers == nil {
		return
	}
	if err := s.qqBadAnswers.AddQQBadAnswer(ctx, qqTruncate(last.Question, 1000), qqTruncate(last.Answer, 2000), qqTruncate(feedback, 200), qqTruncate(retry, 2000)); err != nil {
		slog.Warn("qq bot: bad answer not stored", "reason", err.Error())
	}
}

// qqListBadAnswers 回复最近被说答错的回答（管理员的「差评」）。
func (s *Server) qqListBadAnswers(ctx context.Context, reply func(string)) {
	if s.qqBadAnswers == nil {
		reply(qqReplyUnavailable)
		return
	}
	list, err := s.qqBadAnswers.RecentQQBadAnswers(ctx, qqBadAnswerWindow, qqBadAnswerList)
	if err != nil {
		slog.Warn("qq bot: bad answers unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return
	}
	if len(list) == 0 {
		reply("最近两周没有人说答错。")
		return
	}
	var b strings.Builder
	b.WriteString("最近两周被说答错的回答（最新的在前）：")
	for _, item := range list {
		fmt.Fprintf(&b, "\n\n%s 问：%s\n答：%s\n群友：%s", item.CreatedAt.In(qqStatsZone).Format("01-02 15:04"), qqTruncate(item.Question, 80), qqTruncate(item.Answer, 80), item.Feedback)
		if item.Retry != "" {
			b.WriteString("\n重答：" + qqTruncate(item.Retry, 80))
		}
	}
	b.WriteString("\n\n答案该补进文档的，可以用「记录答案 问：… 答：…」写进 FAQ。")
	reply(b.String())
}
