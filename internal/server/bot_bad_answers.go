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
	"unicode"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// 答错了能被记下来。原来机器人答错了没人知道，只能靠管理员在群里看到。现在群友在追问的时间范围里回一句「不对」「答错了」「没用」这类短话，机器人把上一轮的问答连同这句反馈记下来（account.QQBadAnswer），强制现查、用强模型把上一个问题重答一次；管理员发「差评」看最近两周还没处理的差评（编号 B7 这样，每条两行），「看 B7」看完整的问答，「写进FAQ B7」把重答写进 FAQ（重答也不对就在后面写「答：……」），「忽略 B7」拿掉；处理过的不再列出。原来列表没有编号、没有处理状态，想补进文档得把问答手抄一遍再发「记录答案」，处理过的下次照样列出来。

const (
	// qqBadAnswerRunes 是算作「说上一个回答不对」的消息长度上限：长消息多半是新问题。
	qqBadAnswerRunes = 30
	// qqBadAnswerList、qqBadAnswerWindow 是「差评」列出的条数和时间范围，qqBadAnswerListRunes 是列表里每段文字的长度。
	qqBadAnswerList      = 10
	qqBadAnswerWindow    = 14 * 24 * time.Hour
	qqBadAnswerListRunes = 40
	// qqRetryPrefix 是重答的开头。
	qqRetryPrefix = "我重新查了一下："
	// qqRetryFailed 是重答失败时记进差评的重答内容。
	qqRetryFailed = "（重答失败）"
	// qqRetryExhausted 是重答过一次、群友还说不对时的回复，见 answerQQQuestion；qqRetryExhaustedUnrecorded 是差评没能记下时的回复，不说已经转给管理员。
	qqRetryExhausted           = "我也没查到更好的答案，已经转给管理员看了～"
	qqRetryExhaustedUnrecorded = "我也没查到更好的答案，可以换个说法再 @我 问一次，或者直接在群里问问大家。"
	// qqBadAnswerMergeWindow 是同一个问题的差评合并成一条的时间范围，见 recordQQBadAnswerAgain。
	qqBadAnswerMergeWindow = 30 * time.Minute
	// qqBadAnswerStoreTimeout 是记一条差评的时限。
	qqBadAnswerStoreTimeout = 3 * time.Second
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

var qqBadAnswerFillerWords = [...]string{"还是", "好像", "感觉", "你说的", "你说得", "你这个", "你的", "这个", "回答", "答案", "还", "你", "的", "啊", "呀", "哦", "嘛", "吧", "呢", "啦", "哈", "哇", "喔", "噢"}

var qqBadAnswerNegativeWords = [...]string{"不对", "答错", "错了", "没用", "不准", "不是这样", "胡说", "瞎说", "乱说", "不行", "没解决", "答非所问", "看不懂"}

// qqStripBadAnswerFiller 删除差评判断中的语气词、标点和空白，保持 qqBadAnswerFiller 的正则语义。
func qqStripBadAnswerFiller(text string) string {
	removed := 0
	for i := 0; i < len(text); {
		if word := qqBadAnswerFillerWordAt(text[i:]); word != "" {
			removed += len(word)
			i += len(word)
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsPunct(r) || unicode.IsSymbol(r) || r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r' {
			removed += size
		}
		i += size
	}
	if removed == 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text) - removed)
	start := 0
	for i := 0; i < len(text); {
		if word := qqBadAnswerFillerWordAt(text[i:]); word != "" {
			out.WriteString(text[start:i])
			i += len(word)
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsPunct(r) || unicode.IsSymbol(r) || r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r' {
			out.WriteString(text[start:i])
			i += size
			start = i
			continue
		}
		i += size
	}
	out.WriteString(text[start:])
	return out.String()
}

func qqBadAnswerFillerWordAt(text string) string {
	for _, word := range qqBadAnswerFillerWords {
		if strings.HasPrefix(text, word) {
			return word
		}
	}
	return ""
}

// qqBadAnswerStore 是 account.Service 里被说答错的回答；没有数据库时为 nil。
type qqBadAnswerStore interface {
	AddQQBadAnswer(ctx context.Context, question, answer, feedback, retry string) error
	UpdateQQBadAnswerFeedback(ctx context.Context, question, feedback string, within time.Duration) (bool, error)
	RecentQQBadAnswers(ctx context.Context, since time.Duration, limit int) ([]account.QQBadAnswer, error)
	OpenQQBadAnswers(ctx context.Context, since time.Duration, limit int) ([]account.QQBadAnswer, int, error)
	QQBadAnswer(ctx context.Context, id int64) (account.QQBadAnswer, error)
	CloseQQBadAnswer(ctx context.Context, id int64, state string) (bool, error)
}

// qqIsBadAnswerFeedback 判断这条消息是不是在说上一个回答不对：有上一轮、上一轮答了（不是答不上），消息很短、不是提问，并且整句只是一句否定（qqBadAnswerPattern），或者以否定开头再接纠正内容（qqBadAnswerLead）。
// qqBadAnswerNegative 判断文本是否是固定的否定短语，或以否定短语和分隔符开头。
func qqBadAnswerNegative(text string) bool {
	for _, word := range qqBadAnswerNegativeWords {
		if !strings.HasPrefix(text, word) {
			continue
		}
		end := len(word)
		if strings.HasPrefix(text[end:], "了") {
			end += len("了")
		}
		if end == len(text) {
			return true
		}
		switch text[end] {
		case ',', ':', ' ':
			return true
		}
		if strings.HasPrefix(text[end:], "，") || strings.HasPrefix(text[end:], "。") || strings.HasPrefix(text[end:], "：") {
			return true
		}
	}
	return false
}

// qqBadAnswerQuestionCandidate 判断差评消息中是否出现固定的提问词，避免短消息反复执行正则扫描。
func qqBadAnswerQuestionCandidate(text string) bool {
	return strings.Contains(text, "怎么") || strings.Contains(text, "如何") || strings.Contains(text, "为什么") || strings.Contains(text, "为啥") || strings.ContainsAny(text, "吗？?")
}

func qqIsBadAnswerFeedback(history []account.QQTurn, text string) bool {
	if len(history) == 0 || history[len(history)-1].Answer == qqUnansweredTurn {
		return false
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > qqBadAnswerRunes || qqBadAnswerQuestionCandidate(text) {
		return false
	}
	return qqBadAnswerNegative(qqStripBadAnswerFiller(text)) || qqBadAnswerNegative(text)
}

// qqRetryQuestion 是重答时交给模型的问题：上一个问题、上一个回答和群友的反馈。
func qqRetryQuestion(last account.QQTurn, feedback string) string {
	return last.Question + "\n\n（群友说上一个回答不对：「" + feedback + "」。上一个回答是：" + last.Answer + "。请重新查资料，换个思路认真回答，不要重复上一个回答里的错误。）"
}

// recordQQBadAnswer 记下被说答错的一轮，出错只记日志。不跟随处理消息的 ctx：重答失败多半是 4 分钟用完了，这时 ctx 已经到期，差评仍要记下；另限 qqBadAnswerStoreTimeout，数据库卡住时不拖住后面的回复。
func (s *Server) recordQQBadAnswer(ctx context.Context, last account.QQTurn, feedback, retry string) {
	if s.qqBadAnswers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqBadAnswerStoreTimeout)
	defer cancel()
	if err := s.qqBadAnswers.AddQQBadAnswer(ctx, qqTruncate(last.Question, 1000), qqTruncate(last.Answer, 2000), qqTruncate(feedback, 200), qqTruncate(retry, 2000)); err != nil {
		slog.Warn("qq bot: bad answer not stored", "reason", err.Error())
	}
}

// recordQQBadAnswerAgain 记下群友对已经重答过的一轮又说的「不对」：qqBadAnswerMergeWindow 内同一个问题已经有还没处理的差评时把这句并进去，没有时（隔得久了、上一条已经处理过或没记下）照 recordQQBadAnswer 新记一条，重答一栏留空。返回是否记下了，回复按它选说法。超时和出错的处理照 recordQQBadAnswer。
func (s *Server) recordQQBadAnswerAgain(ctx context.Context, last account.QQTurn, feedback string) bool {
	if s.qqBadAnswers == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqBadAnswerStoreTimeout)
	defer cancel()
	question, feedback := qqTruncate(last.Question, 1000), qqTruncate(feedback, 200)
	merged, err := s.qqBadAnswers.UpdateQQBadAnswerFeedback(ctx, question, feedback, qqBadAnswerMergeWindow)
	if err != nil {
		slog.Warn("qq bot: bad answer not stored", "reason", err.Error())
		return false
	}
	if merged {
		return true
	}
	if err := s.qqBadAnswers.AddQQBadAnswer(ctx, question, qqTruncate(last.Answer, 2000), feedback, ""); err != nil {
		slog.Warn("qq bot: bad answer not stored", "reason", err.Error())
		return false
	}
	return true
}

// qqBadAnswerRetry 是差评里能写进 FAQ 的重答：没重答或者重答失败时为空。
func qqBadAnswerRetry(item account.QQBadAnswer) string {
	if item.Retry == qqRetryFailed {
		return ""
	}
	return strings.TrimSpace(item.Retry)
}

// qqListBadAnswers 回复最近还没处理的差评（管理员的「差评」）：总数，最新的 qqBadAnswerList 条，每条两行：编号、时间、问题和群友的原话；重答（重答失败时是原来的回答）。
func (s *Server) qqListBadAnswers(ctx context.Context, reply func(string)) {
	if s.qqBadAnswers == nil {
		reply(qqReplyUnavailable)
		return
	}
	list, total, err := s.qqBadAnswers.OpenQQBadAnswers(ctx, qqBadAnswerWindow, qqBadAnswerList)
	if err != nil {
		slog.Warn("qq bot: bad answers unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return
	}
	if len(list) == 0 {
		reply("最近两周没有待处理的差评。")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "最近两周被说答错、还没处理的共 %d 条", total)
	if total > len(list) {
		fmt.Fprintf(&b, "，先列最新的 %d 条", len(list))
	}
	b.WriteString("（「看 B编号」看全文，「写进FAQ B编号」把重答写进 FAQ，重答也不对就在后面写「答：……」，「忽略 B编号」拿掉）：")
	for _, item := range list {
		fmt.Fprintf(&b, "\n- B%d %s 问：%s（群友：%s）", item.ID, item.CreatedAt.In(qqStatsZone).Format("01-02 15:04"), qqTruncate(item.Question, qqBadAnswerListRunes), qqTruncate(item.Feedback, qqBadAnswerListRunes))
		if retry := qqBadAnswerRetry(item); retry != "" {
			b.WriteString("\n  重答：" + qqTruncate(retry, qqBadAnswerListRunes))
		} else {
			b.WriteString("\n  重答失败，原来答的：" + qqTruncate(item.Answer, qqBadAnswerListRunes))
		}
	}
	reply(b.String())
}

// qqBadAnswerCommand 处理差评的「看 B编号」「写进FAQ B编号」「忽略 B编号」（action 是 view、write、ignore），只有管理员能用；answer 是「写进FAQ B7 答：……」里管理员写的答案，没写时用重答。返回处理结果的名字。
func (s *Server) qqBadAnswerCommand(ctx context.Context, action string, id int64, answer string, admin bool, reply func(string)) string {
	if !admin {
		reply("只有管理员能处理差评。觉得我答错了，@我 说一句「不对」，我会重新查。")
		return "cmd_bad_answer"
	}
	if s.qqBadAnswers == nil {
		reply(qqReplyUnavailable)
		return "cmd_bad_answer"
	}
	code := "B" + strconv.FormatInt(id, 10)
	item, err := s.qqBadAnswers.QQBadAnswer(ctx, id)
	switch {
	case errors.Is(err, account.ErrQQBadAnswerNotFound):
		reply("没有找到 " + code + "，可能已经处理过或过期了。")
		return "cmd_bad_answer"
	case err != nil:
		slog.Warn("qq bot: bad answer unavailable", "reason", err.Error())
		reply(qqReplyUnavailable)
		return "cmd_bad_answer"
	}
	switch action {
	case "view":
		reply(qqBadAnswerDetail(item, code))
		return "cmd_bad_answer_view"
	case "ignore":
		ok, err := s.qqBadAnswers.CloseQQBadAnswer(ctx, id, "ignored")
		switch {
		case err != nil:
			slog.Warn("qq bot: bad answer not closed", "reason", err.Error())
			reply(qqReplyUnavailable)
			return "cmd_bad_answer"
		case !ok:
			reply(code + " 已经处理过了。")
			return "cmd_bad_answer"
		}
		reply("已忽略 " + code + "。")
		return "cmd_bad_answer_ignore"
	}
	if answer == "" {
		answer = qqBadAnswerRetry(item)
	}
	if answer == "" {
		reply(code + " 重答失败了，没有能写进去的答案，请写成「写进FAQ " + code + " 答：……」。")
		return "cmd_bad_answer"
	}
	pull := s.recordQQAnswer(ctx, "问："+item.Question+"\n答："+answer, reply)
	if pull > 0 {
		// 不跟随处理消息的 ctx：PR 已经开了，ctx 这时到期也要标成已处理，免得下次「差评」还列出来。
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqBadAnswerStoreTimeout)
		defer cancel()
		if _, err := s.qqBadAnswers.CloseQQBadAnswer(sctx, id, "done"); err != nil {
			slog.Warn("qq bot: bad answer not closed", "reason", err.Error())
		}
	}
	return "cmd_bad_answer_write"
}

// qqBadAnswerDetail 是「看 B编号」的回复：问题、原来的回答、群友的原话、重答（各截到 qqFAQCandidateRunes 字）和下一步怎么发。
func qqBadAnswerDetail(item account.QQBadAnswer, code string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s（%s）", code, item.CreatedAt.In(qqStatsZone).Format("01-02 15:04"))
	b.WriteString("\n问：" + qqTruncate(item.Question, qqFAQCandidateRunes))
	b.WriteString("\n原来答的：" + qqTruncate(item.Answer, qqFAQCandidateRunes))
	b.WriteString("\n群友：" + item.Feedback)
	if retry := qqBadAnswerRetry(item); retry != "" {
		b.WriteString("\n重答：" + qqTruncate(retry, qqFAQCandidateRunes))
		b.WriteString("\n照重答写进 FAQ 就 @我 发「写进FAQ " + code + "」，重答也不对就在后面写「答：……」，不用处理就发「忽略 " + code + "」。")
	} else {
		b.WriteString("\n重答：（重答失败）")
		b.WriteString("\n补上答案写进 FAQ 就 @我 发「写进FAQ " + code + " 答：……」，不用处理就发「忽略 " + code + "」。")
	}
	return b.String()
}
