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

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/boundedio"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
	"github.com/metasequoiaime/MSIME-Backend/internal/httpjson"
)

// Telegram 群答疑。admin.telegram 的 Bot 在 telegram_bot.chats 列出的群里被 @、被回复或收到「/命令@Bot用户名」时，把消息转成 botMessage，交给和 QQ 群机器人共用的 handleBotMessage：命令、额度、过渡语和知识库答疑都与 QQ 一致（见 group_bot.go）。Telegram 以 Webhook 把消息推到 telegramBotEventsPath，服务端校验 secret_token、按 update_id 在所有副本间去重后立即应答，再在后台处理，回复引用原消息。图片（照片、当作文件发的图片和被回复消息里的图片）用 getFile 下载后交给看图模型；群主、群管理员和 telegram_bot.admins 里的人是管理员，能提 issue、处理草稿和文档缺口。群友的原话、昵称和 Telegram ID 不写进日志。

const (
	telegramBotEventsPath    = "/v1/community/telegram-bot/events"
	telegramBotBodyBytes     = 64 << 10
	defaultTelegramSecretEnv = "MSIME_TELEGRAM_WEBHOOK_SECRET"
	// telegramUpdateWindow 是 update_id 的去重期。Telegram 在没有收到 2xx 时会重推同一个 update，重推间隔远小于这个时长。
	telegramUpdateWindow = 7 * 24 * time.Hour
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
	// Admins 是额外认作管理员的 Telegram 用户 ID。群主和群管理员由 getChatMember 自动识别，开着「保持匿名」的管理员以群的身份发言时也认作管理员，这份名单只用于不在群管理员里、又要提 issue 的人。
	Admins []int64 `json:"admins"`
	// WebhookSecretEnv 是保存 Webhook secret_token 的环境变量名，默认 MSIME_TELEGRAM_WEBHOOK_SECRET。调用 setWebhook 时传同一个值，Telegram 会在每次回调的 X-Telegram-Bot-Api-Secret-Token 头里带上它。
	WebhookSecretEnv string `json:"webhook_secret_env"`
	secret           string
}

func (c *TelegramBotConfig) enabled() bool { return len(c.Chats) > 0 }

func (c *TelegramBotConfig) validate(cfg *Config) error {
	if !c.enabled() {
		if c.Username != "" || c.WebhookSecretEnv != "" || len(c.Admins) > 0 {
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
	for _, admin := range c.Admins {
		if admin <= 0 {
			return errors.New("telegram_bot admins must be numeric user ids")
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
	// getFile 是 Bot API 的 getFile 地址，files 是下载文件的地址前缀（后面接 getFile 返回的 file_path）。两者都带着令牌，不写进日志和错误。
	getFile, files string
	// fileClient 下载图片，时限比发消息长。
	fileClient *http.Client
	botID      int64
	mention    *regexp.Regexp
	// command 匹配开头的「/命令@Bot用户名」，第一个分组是「/命令」。
	command *regexp.Regexp
	client  *http.Client
	// getChatMember 和 setMyCommands 是 Bot API 的地址，带着令牌。
	getChatMember, setMyCommands string
	// roles 缓存 getChatMember 查到的身份，键是「群/用户」，由 mu 保护，见 isAdmin。
	mu    sync.Mutex
	roles map[string]telegramRole
	now   func() time.Time
}

// telegramRole 是缓存的群身份和查到的时间。
type telegramRole struct {
	admin bool
	at    time.Time
}

// telegramRoleTTL 是 getChatMember 结果在每个副本缓存的时长，和 QQ 的群身份缓存一样。
const telegramRoleTTL = 10 * time.Minute

func newTelegramBot(c TelegramBotConfig, t AdminTelegramConfig) *telegramBot {
	id, _ := strconv.ParseInt(strings.SplitN(t.botToken, ":", 2)[0], 10, 64)
	base := strings.TrimRight(t.APIURL, "/")
	return &telegramBot{
		config:   c,
		endpoint: base + "/bot" + t.botToken + "/sendMessage",
		getFile:  base + "/bot" + t.botToken + "/getFile",
		// 命令菜单和群身份查询。
		getChatMember: base + "/bot" + t.botToken + "/getChatMember",
		setMyCommands: base + "/bot" + t.botToken + "/setMyCommands",
		now:           time.Now,
		files:         base + "/file/bot" + t.botToken + "/",
		fileClient:    httpclient.New(qqVisionTimeout),
		botID:         id,
		// 用户名不区分大小写；前后不能紧挨着用户名里允许的字符，免得 @msime_bot_fan 这样的别人被当成 @ 了机器人。
		mention: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])@` + regexp.QuoteMeta(c.Username) + `($|[^A-Za-z0-9_])`),
		command: regexp.MustCompile(`(?i)^\s*(/[A-Za-z0-9_]+)@` + regexp.QuoteMeta(c.Username) + `($|[^A-Za-z0-9_])`),
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
	// Caption 是图片和文件的说明文字，群友在里面 @ 机器人；Photo 是同一张图片的几种尺寸，从小到大；Document 是当作文件发的附件，只看图片。
	Caption  string              `json:"caption"`
	Photo    []telegramPhotoSize `json:"photo"`
	Document *telegramDocument   `json:"document"`
	// SenderChat 是以群或频道身份发言时的那个 chat：开了「保持匿名」的管理员发言时，from 是 GroupAnonymousBot（is_bot 为 true），sender_chat 是群本身。
	SenderChat *telegramChat `json:"sender_chat"`
}

type telegramPhotoSize struct {
	FileID string `json:"file_id"`
}

type telegramDocument struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

// telegramChannel 是一条 Telegram 群消息的回复通道：sendMessage 引用原消息，次数不限，链接照常发；管理员见 telegramBot.isAdmin。member 是发消息的人，匿名管理员是群本身。
type telegramChannel struct {
	bot                   *telegramBot
	chat, message, member int64
}

func (c telegramChannel) reply(ctx context.Context, _ botMessage, _ int, text string) error {
	return c.bot.reply(ctx, c.chat, c.message, text)
}

func (telegramChannel) maxReplies() int    { return 0 }
func (telegramChannel) rejectsLinks() bool { return false }
func (c telegramChannel) isAdmin(ctx context.Context, _ botMessage) bool {
	return c.bot.isAdmin(ctx, c.chat, c.member)
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
	m, ok := s.tg.message(u.Message)
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
		s.handleBotMessage(ctx, m)
	}()
	return true
}

// message 判断一条消息是不是在问机器人，是的话转成 botMessage：去掉 @，「/ver@Bot用户名」这样带用户名的命令写成「/ver」。回复别人的消息时，那条消息作为引用，和 QQ 的引用消息一样处理：只 @ 了机器人、没写话时把它当成问题，否则接在问题后面；被回复的消息没有文字（图片、贴纸）时注明看不到。回复机器人自己的消息不算引用，上一轮的问答已经在对话历史里。
func (b *telegramBot) message(m *telegramMessage) (botMessage, bool) {
	if m == nil || m.From == nil || (m.Chat.Type != "group" && m.Chat.Type != "supergroup") {
		return botMessage{}, false
	}
	// 匿名管理员以群的身份发言，按发言的群（sender_chat）计作提问者，群里所有匿名管理员共用一份个人额度和对话历史；其他 Bot 和以别的频道身份发的消息不处理。曾经凡是 is_bot 都忽略，开着匿名的管理员 @ 机器人一直没有回应。
	member := m.From.ID
	if m.From.IsBot {
		if m.SenderChat == nil || m.SenderChat.ID != m.Chat.ID {
			return botMessage{}, false
		}
		member = m.SenderChat.ID
	}
	if !slices.Contains(b.config.Chats, m.Chat.ID) {
		// 记下被拦下的群 ID，方便把要接受的群填进 telegram_bot.chats；群 ID 不是个人信息。
		slog.Info("telegram bot: message from unlisted chat", "chat", m.Chat.ID)
		return botMessage{}, false
	}
	repliedToBot := m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == b.botID
	text, command := m.messageText(), false
	if loc := b.command.FindStringSubmatchIndex(text); loc != nil {
		text, command = text[:loc[3]]+text[loc[1]:], true
	} else if name, _, ok := qqSlashParse(text); ok && slices.Contains(telegramPlainCommands, name) {
		// 群里只有这一个 Bot 时，从「/」菜单点出来的命令不带用户名。只认机器人自己的命令名，「/Library/…」这类路径不算。
		command = true
	}
	if !command && !b.mention.MatchString(text) && !repliedToBot {
		return botMessage{}, false
	}
	text = strings.TrimSpace(b.mention.ReplaceAllString(text, "$1$2"))
	chat := strconv.FormatInt(m.Chat.ID, 10)
	out := botMessage{
		ID: "tg:" + chat + ":" + strconv.FormatInt(m.MessageID, 10), Text: text, Group: "tg:" + chat, Member: "tg:" + strconv.FormatInt(member, 10),
		Attachments: b.attachments(m), Platform: botPlatformTelegram, channel: telegramChannel{bot: b, chat: m.Chat.ID, message: m.MessageID, member: member},
	}
	if m.ReplyTo != nil && !repliedToBot {
		out.Quote = qqQuote{Text: qqTruncate(strings.TrimSpace(m.ReplyTo.messageText()), qqQuoteRunes), Images: b.attachments(m.ReplyTo), Referenced: true}
	}
	return out, true
}

// messageText 是消息的文字：文字消息的正文，或者图片、文件的说明文字。
func (m *telegramMessage) messageText() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

// attachments 是消息里的图片：照片取最大的那个尺寸（Telegram 压缩过的 JPEG），当作文件发的只收 image/* 类型。附件只记 file_id，下载见 fetchFile。
func (b *telegramBot) attachments(m *telegramMessage) []qqAttachment {
	var out []qqAttachment
	if n := len(m.Photo); n > 0 && m.Photo[n-1].FileID != "" {
		out = append(out, qqAttachment{ContentType: "image/jpeg", telegramFile: m.Photo[n-1].FileID})
	}
	if d := m.Document; d != nil && d.FileID != "" && qqImageContentType(d.MimeType) {
		out = append(out, qqAttachment{ContentType: d.MimeType, telegramFile: d.FileID})
	}
	return out
}

// telegramFilePath 是 getFile 返回的 file_path 允许的形状，拼进下载地址前先检查，免得被拼出别的路径。
var telegramFilePath = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:/[A-Za-z0-9_.-]+)*$`)

// telegramPlainCommands 是不带「@Bot用户名」也认作发给机器人的斜杠命令，和 qqSlashCommand 认识的命令一致。
var telegramPlainCommands = []string{"help", "h", "ver", "version", "today", "docs", "mcp", "doc", "issues", "add", "stats", "bind", "unbind",
	"drafts", "darfts", "draft", "submit", "undraft", "issue", "faqs", "q", "a", "bad", "weekly"}

// telegramMenuCommands 是注册到群聊「/」菜单的命令（setMyCommands），只列群友用得上的；管理员命令照样能用，见帮助。
var telegramMenuCommands = []map[string]string{
	{"command": "help", "description": "怎么提问和全部命令"},
	{"command": "ver", "description": "各平台最新版本"},
	{"command": "today", "description": "今天的农历、节气、假期和调休"},
	{"command": "docs", "description": "使用文档和下载地址"},
	{"command": "issues", "description": "查有没有人报过，例如 /issues 候选框 左上角"},
	{"command": "add", "description": "投稿词条，例如 /add 扛把子 kang ba zi"},
	{"command": "doc", "description": "文档缺了什么或写错了"},
	{"command": "mcp", "description": "查词库、注音、释义和公共数据的用法"},
}

// registerCommands 把 telegramMenuCommands 注册成所有群聊的命令菜单。启动时调一次，多个副本重复注册结果相同；失败只记日志。
func (b *telegramBot) registerCommands(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, qqBotReplyTimeout)
	defer cancel()
	if err := b.call(ctx, b.setMyCommands, map[string]any{"commands": telegramMenuCommands, "scope": map[string]string{"type": "all_group_chats"}}, nil); err != nil {
		slog.Warn("telegram bot: commands not registered", "reason", err.Error())
	}
}

// isAdmin 判断用户是不是这个群的管理员：匿名管理员以群的身份发言（member 就是群）时一定是；telegram_bot.admins 里的用户是；其余用 getChatMember 查，群主（creator）和管理员（administrator）是。查询结果按副本缓存 telegramRoleTTL；查询出错时沿用过期的缓存，从没查到过才按群友处理。
func (b *telegramBot) isAdmin(ctx context.Context, chat, member int64) bool {
	if member == chat || slices.Contains(b.config.Admins, member) {
		return true
	}
	key := strconv.FormatInt(chat, 10) + "/" + strconv.FormatInt(member, 10)
	b.mu.Lock()
	cached, ok := b.roles[key]
	b.mu.Unlock()
	if ok && b.now().Sub(cached.at) < telegramRoleTTL {
		return cached.admin
	}
	var result struct {
		Status string `json:"status"`
	}
	ctx, cancel := context.WithTimeout(ctx, qqBotReplyTimeout)
	defer cancel()
	if err := b.call(ctx, b.getChatMember, map[string]int64{"chat_id": chat, "user_id": member}, &result); err != nil {
		slog.Warn("telegram bot: member role unavailable", "reason", err.Error())
		return cached.admin
	}
	admin := result.Status == "creator" || result.Status == "administrator"
	b.mu.Lock()
	if b.roles == nil {
		b.roles = map[string]telegramRole{}
	}
	b.roles[key] = telegramRole{admin: admin, at: b.now()}
	b.mu.Unlock()
	return admin
}

// call 调用一个 Bot API 方法，Bot API 回 ok 才算成功，result 不为 nil 时解码返回的 result。错误只带状态和数字错误码，不含令牌或上游描述。
func (b *telegramBot) call(ctx context.Context, endpoint string, payload, result any) error {
	req, err := httpclient.NewJSONPayloadRequest(ctx, http.MethodPost, endpoint, payload)
	if err != nil {
		return errors.New("telegram: invalid endpoint")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New("telegram: request failed")
	}
	defer resp.Body.Close()
	var answer struct {
		OK        bool            `json:"ok"`
		ErrorCode int             `json:"error_code"`
		Result    json.RawMessage `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer) != nil || resp.StatusCode != http.StatusOK || !answer.OK {
		return fmt.Errorf("telegram: Bot API answered %d (code %d)", resp.StatusCode, answer.ErrorCode)
	}
	if result != nil && json.Unmarshal(answer.Result, result) != nil {
		return errors.New("telegram: invalid Bot API response")
	}
	return nil
}

// fetchFile 下载一个 Telegram 文件：先用 getFile 换 file_path，再从文件地址下载，不超过 limit 字节。Bot API 只能下载 20 MB 以内的文件。错误只带状态和数字错误码，不含令牌、地址或上游描述。
func (b *telegramBot) fetchFile(ctx context.Context, fileID string, limit int64) ([]byte, error) {
	var file struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := b.call(ctx, b.getFile, map[string]string{"file_id": fileID}, &file); err != nil {
		return nil, err
	}
	if file.FileSize > limit {
		return nil, errors.New("attachment too large")
	}
	if !telegramFilePath.MatchString(file.FilePath) || strings.Contains(file.FilePath, "..") {
		return nil, errors.New("telegram: invalid file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.files+file.FilePath, nil)
	if err != nil {
		return nil, errors.New("telegram: invalid endpoint")
	}
	resp, err := b.fileClient.Do(req)
	if err != nil {
		return nil, errors.New("attachment download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachment download answered %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, errors.New("attachment too large")
	}
	body, err := httpclient.ReadResponseBody(resp, limit)
	if err != nil {
		return nil, errors.New("attachment download failed")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("attachment too large")
	}
	return body, nil
}

// reply 用 sendMessage 回复群里的一条消息，纯文本、不展开链接预览；被回复的消息已经删掉时照常发出。成功的标准是 Bot API 回 ok，错误只带状态和数字错误码，不含令牌或上游描述。
func (b *telegramBot) reply(ctx context.Context, chat, message int64, text string) error {
	return b.call(ctx, b.endpoint, map[string]any{
		"chat_id":              chat,
		"text":                 telegramText(account.Notice{Title: text}),
		"reply_parameters":     map[string]any{"message_id": message, "allow_sending_without_reply": true},
		"link_preview_options": map[string]bool{"is_disabled": true},
	}, nil)
}
