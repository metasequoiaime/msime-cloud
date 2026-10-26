package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

// Telegram 群答疑。admin.telegram 的 Bot 在 telegram_bot.chats 列出的群里被 @ 或被回复时，用和 QQ 群机器人同一套知识库答疑（answerQQQuestion：挑段落、现查 GitHub、追问接上文、「不对」重答、答不上记文档缺口、报问题起草稿）。Telegram 以 Webhook 把消息推到 telegramBotEventsPath，服务端校验 secret_token、按 update_id 在所有副本间去重后立即应答，再在后台处理，回复引用原消息。只处理文字消息，不支持 QQ 机器人的命令、看截图和管理员提 issue。群友的原话、昵称和 Telegram ID 不写进日志。

const (
	telegramBotEventsPath    = "/v1/community/telegram-bot/events"
	telegramBotBodyBytes     = 64 << 10
	defaultTelegramSecretEnv = "MSIME_TELEGRAM_WEBHOOK_SECRET"
	// telegramUpdateWindow 是 update_id 的去重期。Telegram 在没有收到 2xx 时会重推同一个 update，重推间隔远小于这个时长。
	telegramUpdateWindow = 7 * 24 * time.Hour
	// telegramReplyUsage 是只 @ 了机器人、没写问题时的回复。不提 QQ 机器人的「帮助」命令：Telegram 上没有命令。
	telegramReplyUsage = "有水杉输入法的使用问题可以 @我 直接问，例如：五笔怎么切换？也可以回复我的消息接着问。"
)

var (
	telegramUsernamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,30}[Bb][Oo][Tt]$`)
	// telegramSecretPattern 是 Telegram 允许的 secret_token 字符（1–256 个字母、数字、下划线和连字符），这里另外要求至少 32 个字符。
	telegramSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
)

// TelegramBotConfig 让 admin.telegram 的 Bot 在 Telegram 群里答疑。chats 为空时功能关闭；设置后其余条件都必须满足，否则服务拒绝启动。
type TelegramBotConfig struct {
	// Chats 是接受提问的群（数字 chat_id，超级群是 -100 开头的负数）。不在名单里的群和私聊一律不处理。
	Chats []int64 `json:"chats"`
	// Username 是 Bot 的用户名（不带 @，例如 msime_bot），用来认出群友 @ 了它。
	Username string `json:"username"`
	// WebhookSecretEnv 是保存 Webhook secret_token 的环境变量名，默认 MSIME_TELEGRAM_WEBHOOK_SECRET。调用 setWebhook 时传同一个值，Telegram 会在每次回调的 X-Telegram-Bot-Api-Secret-Token 头里带上它。
	WebhookSecretEnv string `json:"webhook_secret_env"`
	secret           string
}

func (c *TelegramBotConfig) enabled() bool { return len(c.Chats) > 0 }

func (c *TelegramBotConfig) validate(cfg *Config) error {
	if !c.enabled() {
		if c.Username != "" || c.WebhookSecretEnv != "" {
			return errors.New("telegram_bot settings require chats")
		}
		return nil
	}
	if !cfg.QQBot.enabled() {
		return errors.New("telegram_bot requires qq_bot: answers reuse the QQ bot's knowledge base, models and limits")
	}
	if !cfg.Admin.Enabled || cfg.Admin.Telegram.ChatID == "" {
		return errors.New("telegram_bot requires admin.telegram: the bot token is admin.telegram.bot_token_env")
	}
	for _, chat := range c.Chats {
		if chat == 0 {
			return errors.New("telegram_bot chats must be numeric chat ids")
		}
	}
	c.Username = strings.TrimPrefix(c.Username, "@")
	if !telegramUsernamePattern.MatchString(c.Username) {
		return errors.New("telegram_bot username must be the bot's username, ending in bot")
	}
	if c.WebhookSecretEnv == "" {
		c.WebhookSecretEnv = defaultTelegramSecretEnv
	}
	c.secret = os.Getenv(c.WebhookSecretEnv)
	if !telegramSecretPattern.MatchString(c.secret) {
		return errors.New("telegram_bot webhook_secret_env must hold 32 to 256 letters, digits, underscores or hyphens")
	}
	return nil
}

// telegramBot 是答疑用到的 Bot 状态：发消息的接口地址、Bot 自己的用户 ID（令牌冒号前的数字，用来认出回复的是它的消息）和认 @ 的正则。
type telegramBot struct {
	config   TelegramBotConfig
	endpoint string
	botID    int64
	mention  *regexp.Regexp
	client   *http.Client
}

func newTelegramBot(c TelegramBotConfig, t AdminTelegramConfig) *telegramBot {
	id, _ := strconv.ParseInt(strings.SplitN(t.botToken, ":", 2)[0], 10, 64)
	return &telegramBot{
		config:   c,
		endpoint: strings.TrimRight(t.APIURL, "/") + "/bot" + t.botToken + "/sendMessage",
		botID:    id,
		// 用户名不区分大小写；前后不能紧挨着用户名里允许的字符，免得 @msime_bot_fan 这样的别人被当成 @ 了机器人。
		mention: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])@` + regexp.QuoteMeta(c.Username) + `($|[^A-Za-z0-9_])`),
		client:  httpclient.New(qqBotReplyTimeout),
	}
}

// telegramUser、telegramChat、telegramMessage 和 telegramUpdate 是 Bot API Update 里用到的字段。
type telegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramMessage struct {
	MessageID int64            `json:"message_id"`
	From      *telegramUser    `json:"from"`
	Chat      telegramChat     `json:"chat"`
	Text      string           `json:"text"`
	ReplyTo   *telegramMessage `json:"reply_to_message"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

// telegramQuestion 是认领后交给后台处理的一条提问。
type telegramQuestion struct {
	chat, member, message int64
	text                  string
}

// telegramBotEvents 处理 POST /v1/community/telegram-bot/events：Telegram 的 Webhook 回调。secret_token 不对返回 401；其余情况尽快返回 200，Telegram 收到非 2xx 才会重推，只有数据库暂时不可用、认领不了 update 时返回 503 让它稍后重推。
func (s *Server) telegramBotEvents(w http.ResponseWriter, r *http.Request) {
	b := s.tg
	if b == nil {
		fail(w, 503, "feature_disabled")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(b.config.secret)) != 1 {
		fail(w, 401, "invalid_secret")
		return
	}
	body, err := boundedio.ReadAll(http.MaxBytesReader(w, r.Body, telegramBotBodyBytes), r.ContentLength, telegramBotBodyBytes)
	if httpjson.IsBodyTooLarge(err) {
		fail(w, 413, "request_too_large")
		return
	}
	if err != nil {
		fail(w, 400, "invalid_body")
		return
	}
	var u telegramUpdate
	if json.Unmarshal(body, &u) != nil {
		fail(w, 400, "invalid_json")
		return
	}
	if !s.acceptTelegramUpdate(r.Context(), u) {
		fail(w, 503, "telegram_unavailable")
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

// acceptTelegramUpdate 认领一条提问并交给后台处理，返回是否应答成功。只认名单里的群中 @ 了机器人或回复机器人消息的文字消息，其余 update（编辑、频道、私聊、别的群、普通聊天）直接应答成功，重推也不会变好。update_id 在 auth_rates 里只能认领一次；数据库不可用时返回 false，让 Telegram 稍后重推。
func (s *Server) acceptTelegramUpdate(ctx context.Context, u telegramUpdate) bool {
	q, ok := s.tg.question(u.Message)
	if !ok {
		return true
	}
	// 先登记后台任务再认领 update_id，原因同 acceptQQMessage：正在关停的副本不认领，Telegram 重推后由其他副本处理。
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.qqWorkers.Add(1)
	s.mu.Unlock()
	switch err := s.limits.RateLimit(ctx, "telegram-bot-update", strconv.FormatInt(u.UpdateID, 10), 1, telegramUpdateWindow); {
	case errors.Is(err, account.ErrLimited):
		s.qqWorkers.Done()
		return true
	case err != nil:
		s.qqWorkers.Done()
		slog.Error("telegram bot: update claim unavailable", "reason", err.Error())
		return false
	}
	go func() {
		defer s.qqWorkers.Done()
		ctx, cancel := context.WithTimeout(s.lifetime, qqBotProcessTimeout)
		defer cancel()
		s.handleTelegramQuestion(ctx, q)
	}()
	return true
}

// question 判断一条消息是不是在问机器人，是的话返回去掉 @ 之后的问题。只 @ 了机器人、没写话但回复了别人的消息时，把被回复的那句当成问题，和 QQ 引用消息的做法一致。
func (b *telegramBot) question(m *telegramMessage) (telegramQuestion, bool) {
	if m == nil || m.From == nil || m.From.IsBot || (m.Chat.Type != "group" && m.Chat.Type != "supergroup") {
		return telegramQuestion{}, false
	}
	if !slices.Contains(b.config.Chats, m.Chat.ID) {
		// 记下被拦下的群 ID，方便把要接受的群填进 telegram_bot.chats；群 ID 不是个人信息。
		slog.Info("telegram bot: message from unlisted chat", "chat", m.Chat.ID)
		return telegramQuestion{}, false
	}
	repliedToBot := m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == b.botID
	mentioned := b.mention.MatchString(m.Text)
	if !mentioned && !repliedToBot {
		return telegramQuestion{}, false
	}
	text := strings.TrimSpace(b.mention.ReplaceAllString(m.Text, "$1$2"))
	if text == "" && !repliedToBot && m.ReplyTo != nil {
		text = strings.TrimSpace(m.ReplyTo.Text)
	}
	return telegramQuestion{chat: m.Chat.ID, member: m.From.ID, message: m.MessageID, text: text}, true
}

// handleTelegramQuestion 在后台答一条已认领的提问，写法照 handleQQFeedback 的答疑部分：同一个群友的消息按到达顺序处理，先过个人和群的额度，8 秒还没答完先回一句「收到，我想一下～」，再交给 answerQQQuestion。额度和 QQ 分开计数，数目相同。不记录消息内容，日志里只有处理结果和失败原因。
func (s *Server) handleTelegramQuestion(ctx context.Context, q telegramQuestion) {
	started, outcome := time.Now(), "failed"
	chat := "tg:" + strconv.FormatInt(q.chat, 10)
	member := "tg:" + strconv.FormatInt(q.member, 10)
	var refunds []func(context.Context) error
	defer func() {
		// 机器人自己出错或被副本关停打断时退回这条消息计过的额度，理由同 handleQQFeedback。
		if kind := qqOutcomeKind(outcome); kind == "failed" || kind == "reply_failed" || errors.Is(ctx.Err(), context.Canceled) {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			for _, refund := range refunds {
				if err := refund(rctx); err != nil {
					slog.Warn("telegram bot: message slot not released", "reason", err.Error())
				}
			}
			cancel()
		}
		slog.Info("telegram bot: handled", "outcome", outcome, "ms", time.Since(started).Milliseconds())
		s.recordQQOutcome(ctx, outcome)
	}()
	defer s.qqLockSubject(ctx, chat+"/"+member)()
	// 回复可能来自主流程和「我想一下」的计时器两个 goroutine，状态由 mu 保护，发送也在锁里做，群里看到的顺序和发送顺序一致。progressed 表示已经发过一句过渡语，一条消息最多一句。
	var (
		mu          sync.Mutex
		sent        int
		progressed  bool
		thinking    *time.Timer
		finished    bool
		timedOut    bool
		replyFailed bool
		chatting    bool
	)
	send := func(text string, interim bool) {
		if thinking != nil {
			thinking.Stop()
		}
		sent++
		switch {
		case text == qqReplyUnavailable && errors.Is(ctx.Err(), context.Canceled):
			text = qqReplyRestarting
		case text == qqReplyUnavailable && qqOutOfTime(ctx):
			text, timedOut = qqReplyTimedOut, true
		}
		sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qqBotReplyTimeout)
		defer cancel()
		if err := s.tg.reply(sendCtx, q.chat, q.message, text); err != nil {
			slog.Warn("telegram bot: reply failed", "reason", err.Error())
			if !interim {
				replyFailed = true
			}
		}
	}
	reply := func(text string) {
		mu.Lock()
		defer mu.Unlock()
		send(text, false)
	}
	progress := func(text string) {
		mu.Lock()
		defer mu.Unlock()
		if !progressed {
			progressed = true
			send(text, true)
		}
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		finished = true
		if thinking != nil {
			thinking.Stop()
		}
		switch {
		case replyFailed:
			outcome = "reply_failed"
		case timedOut:
			outcome = "timeout"
		}
	}()
	switch n := utf8.RuneCountInString(q.text); {
	case n == 0:
		reply(telegramReplyUsage)
		outcome = "empty"
		return
	case n > qqBotMaxRunes:
		reply(qqReplyTooLong)
		outcome = "too_long"
		return
	}
	for _, limit := range []struct {
		scope, subject string
		daily          int
		text           func(time.Duration) string
	}{{"telegram-bot-member", chat + "/" + member, qqBotMemberDaily, qqLimitedReply}, {"telegram-bot-group", chat, qqBotGroupDaily, qqGroupLimitedReply}} {
		refund, wait, err := s.reserveRateLimit(ctx, limit.scope, limit.subject, limit.daily, 24*time.Hour)
		switch {
		case errors.Is(err, account.ErrLimited):
			reply(limit.text(wait))
			outcome = "limited_" + strings.TrimPrefix(limit.scope, "telegram-bot-")
			return
		case err != nil:
			slog.Error("telegram bot: rate limit unavailable", "reason", err.Error())
			reply(qqReplyUnavailable)
			return
		}
		refunds = append(refunds, refund)
	}
	mu.Lock()
	thinking = time.AfterFunc(qqThinkingDelay, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished && sent == 0 && !progressed && !chatting {
			progressed = true
			send(qqReplyThinking, true)
		}
	})
	mu.Unlock()
	// 判成闲聊时停掉「我想一下」的计时器，理由同 handleQQFeedback。
	onChat := func() {
		mu.Lock()
		defer mu.Unlock()
		chatting = true
		if thinking != nil {
			thinking.Stop()
		}
	}
	outcome = s.answerQQQuestion(ctx, chat, member, q.text, false, reply, progress, onChat)
}

// reply 用 sendMessage 回复群里的一条消息，纯文本、不展开链接预览；被回复的消息已经删掉时照常发出。成功的标准是 Bot API 回 ok，错误只带状态和数字错误码，不含令牌或上游描述。
func (b *telegramBot) reply(ctx context.Context, chat, message int64, text string) error {
	req, err := httpclient.NewJSONPayloadRequest(ctx, http.MethodPost, b.endpoint, map[string]any{
		"chat_id":              chat,
		"text":                 telegramText(account.Notice{Title: text}),
		"reply_parameters":     map[string]any{"message_id": message, "allow_sending_without_reply": true},
		"link_preview_options": map[string]bool{"is_disabled": true},
	})
	if err != nil {
		return errors.New("telegram: invalid endpoint")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New("telegram: request failed")
	}
	defer resp.Body.Close()
	var answer struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer) != nil || resp.StatusCode != http.StatusOK || !answer.OK {
		return fmt.Errorf("telegram: sendMessage answered %d (code %d)", resp.StatusCode, answer.ErrorCode)
	}
	return nil
}
