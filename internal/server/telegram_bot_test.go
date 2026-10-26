package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testTelegramSecret = "synthetic-webhook-secret-0123456789abcdef"
	testTelegramGroup  = -1001234567890
	// testTelegramBotID 是 testTelegramToken 冒号前的数字，即 Bot 自己的用户 ID。
	testTelegramBotID = 123456
)

func TestTelegramBotConfigValidation(t *testing.T) {
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	t.Setenv("TEST_TG_TOKEN", testTelegramToken)
	t.Setenv("TEST_TG_SECRET", testTelegramSecret)
	valid := func() Config {
		c := qqValidConfig()
		c.Admin.Telegram = AdminTelegramConfig{ChatID: "@synthetic_news", BotTokenEnv: "TEST_TG_TOKEN"}
		c.TelegramBot = TelegramBotConfig{Chats: []int64{testTelegramGroup}, Username: "@Msime_Bot", WebhookSecretEnv: "TEST_TG_SECRET"}
		return c
	}
	c := valid()
	if err := c.Admin.Telegram.validate(); err != nil {
		t.Fatal(err)
	}
	if err := c.QQBot.validate(&c); err != nil {
		t.Fatal(err)
	}
	if err := c.TelegramBot.validate(&c); err != nil || c.TelegramBot.Username != "Msime_Bot" || c.TelegramBot.secret != testTelegramSecret {
		t.Fatal(err, c.TelegramBot)
	}
	for name, tc := range map[string]struct {
		edit func(*Config)
		want string
	}{
		"没有群却写了其他设置":        {func(c *Config) { c.TelegramBot.Chats = nil }, "require chats"},
		"没有 QQ 机器人":         {func(c *Config) { c.QQBot = QQBotConfig{} }, "requires qq_bot"},
		"没有 admin.telegram": {func(c *Config) { c.Admin.Telegram = AdminTelegramConfig{} }, "requires admin.telegram"},
		"群 ID 为 0":          {func(c *Config) { c.TelegramBot.Chats = []int64{0} }, "numeric chat ids"},
		"管理员 ID 不对":         {func(c *Config) { c.TelegramBot.Admins = []int64{-5} }, "numeric user ids"},
		"没有群却写了管理员":         {func(c *Config) { c.TelegramBot = TelegramBotConfig{Admins: []int64{7}} }, "require chats"},
		"用户名不是 Bot":         {func(c *Config) { c.TelegramBot.Username = "msime_user" }, "ending in bot"},
		"密钥太短":              {func(c *Config) { t.Setenv("TEST_TG_SECRET", "short") }, "webhook_secret_env"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_TG_SECRET", testTelegramSecret)
			c := valid()
			tc.edit(&c)
			if err := c.TelegramBot.validate(&c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
	// 关闭时什么都不要求；默认的密钥变量名是 MSIME_TELEGRAM_WEBHOOK_SECRET。
	off := Config{}
	if err := off.TelegramBot.validate(&off); err != nil {
		t.Fatal(err)
	}
	t.Setenv(defaultTelegramSecretEnv, testTelegramSecret)
	c = valid()
	c.TelegramBot.WebhookSecretEnv = ""
	if err := c.TelegramBot.validate(&c); err != nil || c.TelegramBot.WebhookSecretEnv != defaultTelegramSecretEnv {
		t.Fatal(err, c.TelegramBot.WebhookSecretEnv)
	}
	// 未知字段让服务拒绝启动，telegram_bot 必须是 Config 认识的字段。
	var decoded Config
	if err := json.Unmarshal([]byte(`{"telegram_bot":{"chats":[-1001],"username":"msime_bot"}}`), &decoded); err != nil || decoded.TelegramBot.Chats[0] != -1001 {
		t.Fatal(err, decoded.TelegramBot)
	}
}

// telegramServer 在 qqServer 上接好 Telegram 答疑，Bot API 由 fakeTelegram 扮演。
func telegramServer(t *testing.T) (*Server, *qqFake, *qqLimiter, *fakeTelegram) {
	t.Helper()
	s, fake, limiter, api := telegramAPIServer(t)
	return s, fake, limiter, api.fakeTelegram
}

// telegramAPIServer 和 telegramServer 一样，另外返回扮演 getFile、getChatMember 和 setMyCommands 的整个 Bot API。
func telegramAPIServer(t *testing.T) (*Server, *qqFake, *qqLimiter, *fakeTelegramFiles) {
	t.Helper()
	s, fake, limiter := qqServer(t)
	api := &fakeTelegramFiles{fakeTelegram: &fakeTelegram{status: 200, ok: true}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	s.tg = newTelegramBot(TelegramBotConfig{Chats: []int64{testTelegramGroup}, Username: "msime_bot", secret: testTelegramSecret}, AdminTelegramConfig{APIURL: srv.URL, botToken: testTelegramToken})
	return s, fake, limiter, api
}

func telegramPost(s *Server, secret, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", telegramBotEventsPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// telegramUpdateJSON 是群友 42 在 chat 里发的一条消息；reply 不为 nil 时是对那条消息的回复。
func telegramUpdateJSON(update int64, chat int64, chatType, text string, reply map[string]any) string {
	message := map[string]any{"message_id": update * 10, "from": map[string]any{"id": 42, "is_bot": false}, "chat": map[string]any{"id": chat, "type": chatType}, "text": text}
	if reply != nil {
		message["reply_to_message"] = reply
	}
	raw, _ := json.Marshal(map[string]any{"update_id": update, "message": message})
	return string(raw)
}

// telegramDeliver 推送一条群消息并等后台处理结束。
func telegramDeliver(t *testing.T, s *Server, update int64, text string, reply map[string]any) {
	t.Helper()
	if w := telegramPost(s, testTelegramSecret, telegramUpdateJSON(update, testTelegramGroup, "supergroup", text, reply)); w.Code != 200 || w.Body.String() != `{"ok":true}`+"\n" {
		t.Fatal(w.Code, w.Body.String())
	}
	s.qqWorkers.Wait()
}

func (f *fakeTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var texts []string
	for _, m := range f.messages {
		texts = append(texts, m["text"].(string))
	}
	return texts
}

// 回调只认带对 secret_token 的请求；未配置时 503，请求体过大 413，不是 JSON 400。
func TestTelegramBotWebhookProtocol(t *testing.T) {
	s, _, _, tg := telegramServer(t)
	for _, secret := range []string{"", "wrong-" + testTelegramSecret} {
		if w := telegramPost(s, secret, `{"update_id":1}`); w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_secret") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := telegramPost(s, testTelegramSecret, `{"update_id":`); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_json") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := telegramPost(s, testTelegramSecret, `{"update_id":1,"pad":"`+strings.Repeat("x", telegramBotBodyBytes)+`"}`); w.Code != 413 {
		t.Fatal(w.Code, w.Body.String())
	}
	// 没有 message 的 update（编辑、成员变动）直接应答成功。
	if w := telegramPost(s, testTelegramSecret, `{"update_id":2,"edited_message":{"text":"x"}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.tg = nil
	if w := telegramPost(s, testTelegramSecret, `{"update_id":3}`); w.Code != 503 || !strings.Contains(w.Body.String(), "feature_disabled") {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(tg.messages) != 0 {
		t.Fatal(tg.messages)
	}
}

// 群友 @ 机器人或回复机器人的消息时按知识库答疑，回复引用原消息；同一个 update 只答一次，其他消息不处理。
func TestTelegramBotAnswersMentionsAndReplies(t *testing.T) {
	s, fake, limiter, tg := telegramServer(t)
	logs := captureLogs(t)
	telegramDeliver(t, s, 7, "@MSIME_BOT 五笔怎么切换", nil)
	if texts := tg.texts(); len(texts) != 1 || texts[0] != "在设置的「输入方案」里选五笔。" || strings.Join(fake.chatCalls, ",") != "select,answer" {
		t.Fatal(texts, fake.chatCalls)
	}
	sent := tg.messages[0]
	params, _ := sent["reply_parameters"].(map[string]any)
	if sent["chat_id"] != float64(testTelegramGroup) || params["message_id"] != float64(70) || params["allow_sending_without_reply"] != true || tg.paths[0] != "POST /bot"+testTelegramToken+"/sendMessage" {
		t.Fatal(sent, tg.paths)
	}
	// 问题去掉了 @；额度和 QQ 共用作用域，按 tg: 开头的群和成员标识分别计数。
	if limiter.counts["qq-bot-member|tg:-1001234567890/tg:42"] != 1 || limiter.counts["qq-bot-group|tg:-1001234567890"] != 1 {
		t.Fatal(limiter.counts)
	}
	if !strings.Contains(logs.String(), `"msg":"qq bot: handled"`) || !strings.Contains(logs.String(), `"platform":"telegram","outcome":"answered"`) || strings.Contains(logs.String(), "五笔") {
		t.Fatal(logs.String())
	}
	// Telegram 重推同一个 update 时不再回答。
	telegramDeliver(t, s, 7, "@msime_bot 五笔怎么切换", nil)
	if len(tg.messages) != 1 {
		t.Fatal("同一个 update 答了两次", tg.texts())
	}
	// 没有 @、@ 的是别人、名单外的群、私聊和 Bot 发的消息都不处理。
	telegramDeliver(t, s, 8, "五笔怎么切换", nil)
	telegramDeliver(t, s, 9, "@msime_bot_fan 五笔怎么切换", nil)
	for i, body := range []string{
		telegramUpdateJSON(10, -1009999, "supergroup", "@msime_bot 五笔怎么切换", nil),
		telegramUpdateJSON(11, 42, "private", "@msime_bot 五笔怎么切换", nil),
		strings.Replace(telegramUpdateJSON(12, testTelegramGroup, "supergroup", "@msime_bot 五笔怎么切换", nil), `"is_bot":false`, `"is_bot":true`, 1),
	} {
		if w := telegramPost(s, testTelegramSecret, body); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	s.qqWorkers.Wait()
	if len(tg.messages) != 1 || !strings.Contains(logs.String(), "telegram bot: message from unlisted chat") || !strings.Contains(logs.String(), `"chat":-1009999`) {
		t.Fatal(tg.texts(), logs.String())
	}
	// 回复机器人的消息不用再 @。
	botMessage := map[string]any{"message_id": 70, "from": map[string]any{"id": testTelegramBotID, "is_bot": true}, "text": "在设置的「输入方案」里选五笔。"}
	telegramDeliver(t, s, 13, "那拼音呢", botMessage)
	if len(tg.messages) != 2 {
		t.Fatal(tg.texts())
	}
	// 只 @ 了机器人、回复的是别人的消息时，把那条消息当成问题；什么都没写时提示怎么问。
	other := map[string]any{"message_id": 5, "from": map[string]any{"id": 99, "is_bot": false}, "text": "双拼怎么开"}
	telegramDeliver(t, s, 14, "@msime_bot", other)
	if len(tg.messages) != 3 || !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "双拼怎么开") {
		t.Fatal(tg.texts())
	}
	telegramDeliver(t, s, 15, "  @msime_bot  ", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqReplyUsage {
		t.Fatal(last)
	}
	telegramDeliver(t, s, 16, "@msime_bot "+strings.Repeat("长", qqBotMaxRunes+1), nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqReplyTooLong {
		t.Fatal(last)
	}
	// 个人额度用完时说清还要等多久，不再计入群的额度。
	limiter.counts["qq-bot-member|tg:-1001234567890/tg:42"] = qqBotMemberDaily
	group := limiter.counts["qq-bot-group|tg:-1001234567890"]
	telegramDeliver(t, s, 17, "@msime_bot 五笔怎么切换", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqLimitedReply(24*time.Hour) || limiter.counts["qq-bot-group|tg:-1001234567890"] != group {
		t.Fatal(last, limiter.counts)
	}
	delete(limiter.counts, "qq-bot-member|tg:-1001234567890/tg:42")
	limiter.counts["qq-bot-group|tg:-1001234567890"] = qqBotGroupDaily
	telegramDeliver(t, s, 18, "@msime_bot 五笔怎么切换", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqGroupLimitedReply(24*time.Hour) {
		t.Fatal(last)
	}
}

// 答得慢时先回一句「收到，我想一下～」；回答没发出去时退回额度，日志里只有状态码，不带令牌。
func TestTelegramBotThinkingAndFailures(t *testing.T) {
	defer func(delay time.Duration) { qqThinkingDelay = delay }(qqThinkingDelay)
	qqThinkingDelay = 20 * time.Millisecond
	s, fake, limiter, tg := telegramServer(t)
	logs := captureLogs(t)
	fake.slowCall, fake.slowDelay = "answer", 300*time.Millisecond
	telegramDeliver(t, s, 21, "@msime_bot 五笔怎么切换", nil)
	if texts := tg.texts(); len(texts) != 2 || texts[0] != qqReplyThinking || texts[1] != "在设置的「输入方案」里选五笔。" {
		t.Fatal(texts)
	}
	// 闲聊不先说「我想一下」。
	fake.slowCall = ""
	fake.selected, fake.answer = `{"sections":[]}`, `{"type":"chat","answer":"在的～"}`
	telegramDeliver(t, s, 22, "@msime_bot 在吗", nil)
	if texts := tg.texts(); len(texts) != 3 || texts[2] != "在的～" {
		t.Fatal(texts)
	}
	member := "qq-bot-member|tg:-1001234567890/tg:42"
	before := limiter.counts[member]
	tg.mu.Lock()
	tg.status, tg.ok = 500, false
	tg.mu.Unlock()
	telegramDeliver(t, s, 23, "@msime_bot 在吗", nil)
	if limiter.counts[member] != before || !strings.Contains(logs.String(), "qq bot: reply failed") || !strings.Contains(logs.String(), "answered 500") || !strings.Contains(logs.String(), `"outcome":"reply_failed"`) || strings.Contains(logs.String(), testTelegramToken) {
		t.Fatal(limiter.counts[member], before, logs.String())
	}
	tg.mu.Lock()
	tg.status, tg.ok = 200, true
	tg.mu.Unlock()
	// 模型出错时回「暂时处理不了」并退回额度。
	fake.chatStatus = 500
	telegramDeliver(t, s, 24, "@msime_bot 五笔怎么切换", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqReplyUnavailable || limiter.counts[member] != before {
		t.Fatal(last, limiter.counts[member], before)
	}
	fake.chatStatus = 0
	// 额度存储出错时也回「暂时处理不了」。
	limiter.err = map[string]error{"qq-bot-member": errors.New("synthetic database failure")}
	telegramDeliver(t, s, 25, "@msime_bot 五笔怎么切换", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqReplyUnavailable || !strings.Contains(logs.String(), "qq bot: rate limit unavailable") {
		t.Fatal(last)
	}
	// 认领 update 的存储出错时返回 503，让 Telegram 稍后重推；关停中的副本同样不认领。
	limiter.err = map[string]error{"telegram-bot-update": errors.New("synthetic database failure")}
	if w := telegramPost(s, testTelegramSecret, telegramUpdateJSON(26, testTelegramGroup, "supergroup", "@msime_bot 在吗", nil)); w.Code != 503 || !strings.Contains(w.Body.String(), "telegram_unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
	limiter.err = nil
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if w := telegramPost(s, testTelegramSecret, telegramUpdateJSON(27, testTelegramGroup, "supergroup", "@msime_bot 在吗", nil)); w.Code != 503 {
		t.Fatal(w.Code)
	}
	s.mu.Lock()
	s.closed = false
	s.mu.Unlock()
}

// 超过 Telegram 单条 4096 字的回复按字符边界截断。
func TestTelegramBotReplyLength(t *testing.T) {
	s, _, _, tg := telegramServer(t)
	if err := s.tg.reply(t.Context(), testTelegramGroup, 1, strings.Repeat("水", telegramMessageMax+10)); err != nil {
		t.Fatal(err)
	}
	if text := tg.texts()[0]; utf8.RuneCountInString(text) != telegramMessageMax || !strings.HasSuffix(text, "…") {
		t.Fatal(utf8.RuneCountInString(text))
	}
	broken := newTelegramBot(TelegramBotConfig{Username: "msime_bot"}, AdminTelegramConfig{APIURL: "http://127.0.0.1:1", botToken: testTelegramToken})
	if err := broken.reply(t.Context(), testTelegramGroup, 1, "x"); err == nil || strings.Contains(err.Error(), testTelegramToken) {
		t.Fatal(err)
	}
	bad := newTelegramBot(TelegramBotConfig{Username: "msime_bot"}, AdminTelegramConfig{APIURL: "http://[::1", botToken: testTelegramToken})
	if err := bad.reply(t.Context(), testTelegramGroup, 1, "x"); err == nil || err.Error() != "telegram: invalid endpoint" {
		t.Fatal(err)
	}
}

// 开着「保持匿名」的管理员以群的身份发言（from 是 GroupAnonymousBot，sender_chat 是群本身）时照常处理，按群计作提问者，并认作管理员（只有管理员能匿名），额度记在管理员名下；以别的频道身份发的消息和其他 Bot 的消息不处理。
func TestTelegramBotAnswersAnonymousAdmins(t *testing.T) {
	s, _, limiter, tg := telegramServer(t)
	anonymous := func(update int64, senderChat any) string {
		raw, _ := json.Marshal(map[string]any{"update_id": update, "message": map[string]any{
			"message_id": update * 10, "from": map[string]any{"id": 1087968824, "is_bot": true},
			"sender_chat": senderChat, "chat": map[string]any{"id": testTelegramGroup, "type": "supergroup"}, "text": "@msime_bot 五笔怎么切换",
		}})
		return string(raw)
	}
	for i, body := range []string{
		anonymous(31, map[string]any{"id": -1005555, "type": "channel"}),
		anonymous(32, nil),
	} {
		if w := telegramPost(s, testTelegramSecret, body); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	s.qqWorkers.Wait()
	if len(tg.messages) != 0 {
		t.Fatal("以别的频道身份或 Bot 发的消息被回答了", tg.texts())
	}
	if w := telegramPost(s, testTelegramSecret, anonymous(33, map[string]any{"id": testTelegramGroup, "type": "supergroup"})); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.qqWorkers.Wait()
	if texts := tg.texts(); len(texts) != 1 || texts[0] != "在设置的「输入方案」里选五笔。" || limiter.counts["qq-bot-admin|tg:-1001234567890"] != 1 {
		t.Fatal(texts, limiter.counts)
	}
}

// Telegram 群里的命令和 QQ 共用一套：@ 之后接命令或写成「/命令@Bot用户名」都认，帮助换成 Telegram 的版本，打字数据提示去 QQ 绑定；回复别人的消息时那条消息接在问题后面，没有文字时注明看不到。
func TestTelegramBotCommandsAndQuotes(t *testing.T) {
	s, fake, _, tg := telegramServer(t)
	telegramDeliver(t, s, 41, "@msime_bot 帮助", nil)
	telegramDeliver(t, s, 42, "/help@msime_bot", nil)
	telegramDeliver(t, s, 43, "@msime_bot 我的数据", nil)
	telegramDeliver(t, s, 44, "@msime_bot 我的ID", nil)
	// 发给别的 Bot 的命令不处理。
	telegramDeliver(t, s, 45, "/help@other_bot", nil)
	texts := tg.texts()
	if len(texts) != 4 || texts[0] != telegramHelpText || texts[1] != telegramHelpText || texts[2] != qqTypingTelegramReply || texts[3] != "你的 ID 是 tg:42，机器人把你当作普通成员。" || len(fake.chatCalls) != 0 {
		t.Fatal(texts, fake.chatCalls)
	}
	if strings.Contains(telegramHelpText, "/stats") || strings.Contains(telegramHelpText, "/bind") || !strings.Contains(qqHelpText, "/stats") {
		t.Fatal("帮助没有按平台区分")
	}
	telegramDeliver(t, s, 46, "@msime_bot 这个怎么开", map[string]any{"message_id": 5, "from": map[string]any{"id": 99, "is_bot": false}, "text": "双拼"})
	if !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "（群友引用的消息：「双拼」）") {
		t.Fatal(fake.chatBodies[len(fake.chatBodies)-1])
	}
	telegramDeliver(t, s, 47, "@msime_bot 这是什么", map[string]any{"message_id": 6, "from": map[string]any{"id": 99, "is_bot": false}})
	if !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "看不到引用的内容") {
		t.Fatal(fake.chatBodies[len(fake.chatBodies)-1])
	}
}

// fakeTelegramFiles 是群答疑测试用的 Bot API：sendMessage 交给 fakeTelegram 记录；getFile 按 files（file_id → file_path、file_size）回答，文件内容见 contents；getChatMember 按 roles（用户 ID → status）回答，没列出的是 member，roleStatus 非 0 时回这个错误状态；setMyCommands 记进 commands。
type fakeTelegramFiles struct {
	*fakeTelegram
	files      map[string]map[string]any
	contents   map[string][]byte
	getFiles   []string
	roles      map[int64]string
	roleStatus int
	roleCalls  int
	commands   []map[string]any
}

func (f *fakeTelegramFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/bot"+testTelegramToken+"/getChatMember":
		var body struct {
			UserID int64 `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.roleCalls++
		status, failure := f.roles[body.UserID], f.roleStatus
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if failure != 0 {
			w.WriteHeader(failure)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": failure, "description": "synthetic " + testTelegramToken})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"status": cmp.Or(status, "member")}})
	case r.URL.Path == "/bot"+testTelegramToken+"/setMyCommands":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.commands = append(f.commands, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": f.ok, "result": true})
	case r.URL.Path == "/bot"+testTelegramToken+"/getFile":
		var body struct {
			FileID string `json:"file_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.getFiles = append(f.getFiles, body.FileID)
		file, ok := f.files[body.FileID]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: invalid file_id " + testTelegramToken})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": file})
	case strings.HasPrefix(r.URL.Path, "/file/bot"+testTelegramToken+"/"):
		content, ok := f.contents[strings.TrimPrefix(r.URL.Path, "/file/bot"+testTelegramToken+"/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(content)
	default:
		f.fakeTelegram.ServeHTTP(w, r)
	}
}

// 照片和当作文件发的图片经 getFile 下载后交给看图模型：说明文字里 @ 机器人、只发图片回复机器人、回复别人的图片再 @ 都会看图；取不到的图片在问题后面注明没看清。日志里没有令牌。
func TestTelegramBotImages(t *testing.T) {
	s, fake, _, tg := telegramAPIServer(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	tg.files = map[string]map[string]any{
		"photo-large": {"file_path": "photos/file_1.png", "file_size": len(png)},
		"doc-png":     {"file_path": "documents/file_2.png", "file_size": len(png)},
		"too-large":   {"file_path": "photos/file_3.png", "file_size": qqVisionBytes + 1},
		"escape":      {"file_path": "../secrets", "file_size": 10},
	}
	tg.contents = map[string][]byte{"photos/file_1.png": png, "documents/file_2.png": png}
	logs := captureLogs(t)
	photo := func(update int64, caption string, photos []string, reply map[string]any) string {
		sizes := []map[string]any{}
		for _, id := range photos {
			sizes = append(sizes, map[string]any{"file_id": id})
		}
		message := map[string]any{"message_id": update * 10, "from": map[string]any{"id": 42, "is_bot": false}, "chat": map[string]any{"id": testTelegramGroup, "type": "supergroup"}, "caption": caption, "photo": sizes}
		if reply != nil {
			message["reply_to_message"] = reply
		}
		raw, _ := json.Marshal(map[string]any{"update_id": update, "message": message})
		return string(raw)
	}
	deliver := func(body string) {
		t.Helper()
		if w := telegramPost(s, testTelegramSecret, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		s.qqWorkers.Wait()
	}
	// 说明文字里 @ 机器人：取最大的尺寸看图，描述接在问题后面。
	deliver(photo(51, "@msime_bot 这是什么报错", []string{"photo-small", "photo-large"}, nil))
	if len(tg.getFiles) != 1 || tg.getFiles[0] != "photo-large" || len(fake.visionCalls) != 1 || !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "这是什么报错") || !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], qqShotMarker) {
		t.Fatal(tg.getFiles, fake.visionCalls, fake.chatBodies)
	}
	// 没有 @ 也没回复机器人的图片不处理。
	deliver(photo(52, "看看这个", []string{"photo-large"}, nil))
	if len(tg.getFiles) != 1 {
		t.Fatal(tg.getFiles)
	}
	// 只发图片回复机器人：按只发了截图处理。
	botMessage := map[string]any{"message_id": 1, "from": map[string]any{"id": testTelegramBotID, "is_bot": true}, "text": "能发个截图吗"}
	deliver(photo(53, "", []string{"photo-large"}, botMessage))
	if len(fake.visionCalls) != 2 || !strings.HasPrefix(fake.chatBodies[len(fake.chatBodies)-1], "（群友只发了截图，没写文字）") {
		t.Fatal(fake.visionCalls, fake.chatBodies[len(fake.chatBodies)-1])
	}
	// 回复别人当作文件发的图片再 @ 机器人：看被回复的那张。PDF 这类文件不算图片。
	other := map[string]any{"message_id": 2, "from": map[string]any{"id": 99, "is_bot": false}, "document": map[string]any{"file_id": "doc-png", "mime_type": "image/png"}}
	telegramDeliver(t, s, 54, "@msime_bot 这个设置在哪", other)
	if len(fake.visionCalls) != 3 || tg.getFiles[len(tg.getFiles)-1] != "doc-png" {
		t.Fatal(fake.visionCalls, tg.getFiles)
	}
	pdf := map[string]any{"message_id": 3, "from": map[string]any{"id": 99, "is_bot": false}, "document": map[string]any{"file_id": "doc-pdf", "mime_type": "application/pdf"}}
	telegramDeliver(t, s, 55, "@msime_bot 这是什么", pdf)
	if len(fake.visionCalls) != 3 || tg.getFiles[len(tg.getFiles)-1] != "doc-png" || !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], "看不到引用的内容") {
		t.Fatal(tg.getFiles, fake.chatBodies[len(fake.chatBodies)-1])
	}
	// getFile 失败、文件超过上限、file_path 不合规时不下载，问题后面注明截图没看清。
	for i, id := range []string{"missing", "too-large", "escape"} {
		deliver(photo(int64(56+i), "@msime_bot 这是什么", []string{id}, nil))
		if len(fake.visionCalls) != 3 || !strings.Contains(fake.chatBodies[len(fake.chatBodies)-1], qqShotMissedNote) {
			t.Fatal(id, fake.visionCalls, fake.chatBodies[len(fake.chatBodies)-1])
		}
	}
	if !strings.Contains(logs.String(), "screenshot not described") || strings.Contains(logs.String(), testTelegramToken) || strings.Contains(logs.String(), "file_1.png") {
		t.Fatal(logs.String())
	}
	// 没有启用 Telegram 时 Telegram 附件下载不了。
	s.tg = nil
	if _, err := s.downloadAttachment(t.Context(), qqAttachment{ContentType: "image/png", telegramFile: "photo-large"}, qqVisionBytes); err == nil {
		t.Fatal("Telegram 关闭时下载了附件")
	}
}

// Telegram 的管理员：群主、群管理员（getChatMember）、telegram_bot.admins 和匿名管理员。身份按副本缓存，查询出错时沿用过期的缓存；管理员能提 issue，issue 页脚写明来自 Telegram 群。不带用户名的菜单命令也认，命令菜单注册到所有群聊。
func TestTelegramBotAdmins(t *testing.T) {
	s, fake, _, api := telegramAPIServer(t)
	logs := captureLogs(t)
	api.roles = map[int64]string{7: "administrator", 8: "creator"}
	s.tg.config.Admins = []int64{99}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s.tg.now = func() time.Time { return now }
	ctx := t.Context()
	for member, want := range map[int64]bool{7: true, 8: true, 42: false} {
		if got := s.tg.isAdmin(ctx, testTelegramGroup, member); got != want {
			t.Fatal(member, got)
		}
	}
	// 名单里的人和匿名管理员不查接口；缓存期内也不再查。
	if !s.tg.isAdmin(ctx, testTelegramGroup, 99) || !s.tg.isAdmin(ctx, testTelegramGroup, testTelegramGroup) || !s.tg.isAdmin(ctx, testTelegramGroup, 7) || api.roleCalls != 3 {
		t.Fatal(api.roleCalls)
	}
	// 缓存过期后查询出错：沿用过期的身份；从没查到过的人按群友处理。
	now = now.Add(telegramRoleTTL + time.Minute)
	api.roleStatus = 500
	if !s.tg.isAdmin(ctx, testTelegramGroup, 7) || s.tg.isAdmin(ctx, testTelegramGroup, 50) || !strings.Contains(logs.String(), "telegram bot: member role unavailable") || strings.Contains(logs.String(), testTelegramToken) {
		t.Fatal(logs.String())
	}
	api.roleStatus = 0
	from := func(update, user int64, text string) {
		t.Helper()
		body := strings.Replace(telegramUpdateJSON(update, testTelegramGroup, "supergroup", text, nil), `"id":42`, `"id":`+strconv.FormatInt(user, 10), 1)
		if w := telegramPost(s, testTelegramSecret, body); w.Code != 200 {
			t.Fatal(w.Code)
		}
		s.qqWorkers.Wait()
	}
	from(61, 7, "@msime_bot 我的ID")
	if last := api.texts()[len(api.messages)-1]; last != "你的 ID 是 tg:7，机器人把你当作管理员。" {
		t.Fatal(last)
	}
	// 管理员提 issue：页脚写明来自 Telegram 群。
	from(62, 7, "@msime_bot 提issue Windows 11 上水杉 0.9.5 候选框停在左上角")
	if len(fake.created) != 1 || !strings.Contains(fake.created[0]["body"].(string), "来自 Telegram 群反馈") {
		t.Fatal(fake.created, api.texts())
	}
	// 群友发的「提issue」不提交。
	from(63, 42, "@msime_bot 提issue Windows 11 上水杉 0.9.5 候选框停在左上角")
	if len(fake.created) != 1 {
		t.Fatal("群友提交了 issue")
	}
	// 菜单里点出来的命令不带用户名；不是机器人命令的斜杠开头的话不处理。
	sent, calls := len(api.messages), len(fake.chatCalls)
	from(64, 42, "/help")
	from(65, 42, "/Library/Input Methods 里没有")
	if len(api.messages) != sent+1 || api.texts()[sent] != telegramHelpText || len(fake.chatCalls) != calls {
		t.Fatal(api.texts()[sent:], fake.chatCalls[calls:])
	}
	// 命令菜单注册到所有群聊；注册失败只记日志。
	s.tg.registerCommands(ctx)
	if len(api.commands) != 1 || api.commands[0]["scope"].(map[string]any)["type"] != "all_group_chats" || len(api.commands[0]["commands"].([]any)) != len(telegramMenuCommands) {
		t.Fatal(api.commands)
	}
	api.mu.Lock()
	api.ok = false
	api.mu.Unlock()
	s.tg.registerCommands(ctx)
	if !strings.Contains(logs.String(), "telegram bot: commands not registered") || strings.Contains(logs.String(), testTelegramToken) {
		t.Fatal(logs.String())
	}
}
