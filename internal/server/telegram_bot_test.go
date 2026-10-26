package server

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
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
	s, fake, limiter := qqServer(t)
	tg := &fakeTelegram{status: 200, ok: true}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)
	s.tg = newTelegramBot(TelegramBotConfig{Chats: []int64{testTelegramGroup}, Username: "msime_bot", secret: testTelegramSecret}, AdminTelegramConfig{APIURL: srv.URL, botToken: testTelegramToken})
	return s, fake, limiter, tg
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
	// 问题去掉了 @，额度按群友和群分别计数，和 QQ 的额度分开。
	if limiter.counts["telegram-bot-member|tg:-1001234567890/tg:42"] != 1 || limiter.counts["telegram-bot-group|tg:-1001234567890"] != 1 || limiter.counts["qq-bot-member|tg:-1001234567890/tg:42"] != 0 {
		t.Fatal(limiter.counts)
	}
	if !strings.Contains(logs.String(), "telegram bot: handled") || !strings.Contains(logs.String(), `"outcome":"answered"`) || strings.Contains(logs.String(), "五笔") {
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
	if last := tg.texts()[len(tg.messages)-1]; last != telegramReplyUsage {
		t.Fatal(last)
	}
	telegramDeliver(t, s, 16, "@msime_bot "+strings.Repeat("长", qqBotMaxRunes+1), nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqReplyTooLong {
		t.Fatal(last)
	}
	// 个人额度用完时说清还要等多久，不再计入群的额度。
	limiter.counts["telegram-bot-member|tg:-1001234567890/tg:42"] = qqBotMemberDaily
	group := limiter.counts["telegram-bot-group|tg:-1001234567890"]
	telegramDeliver(t, s, 17, "@msime_bot 五笔怎么切换", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqLimitedReply(24*time.Hour) || limiter.counts["telegram-bot-group|tg:-1001234567890"] != group {
		t.Fatal(last, limiter.counts)
	}
	delete(limiter.counts, "telegram-bot-member|tg:-1001234567890/tg:42")
	limiter.counts["telegram-bot-group|tg:-1001234567890"] = qqBotGroupDaily
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
	member := "telegram-bot-member|tg:-1001234567890/tg:42"
	before := limiter.counts[member]
	tg.mu.Lock()
	tg.status, tg.ok = 500, false
	tg.mu.Unlock()
	telegramDeliver(t, s, 23, "@msime_bot 在吗", nil)
	if limiter.counts[member] != before || !strings.Contains(logs.String(), "telegram bot: reply failed") || !strings.Contains(logs.String(), "answered 500") || !strings.Contains(logs.String(), `"outcome":"reply_failed"`) || strings.Contains(logs.String(), testTelegramToken) {
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
	limiter.err = map[string]error{"telegram-bot-member": errors.New("synthetic database failure")}
	telegramDeliver(t, s, 25, "@msime_bot 五笔怎么切换", nil)
	if last := tg.texts()[len(tg.messages)-1]; last != qqReplyUnavailable || !strings.Contains(logs.String(), "telegram bot: rate limit unavailable") {
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

// 开着「保持匿名」的管理员以群的身份发言（from 是 GroupAnonymousBot，sender_chat 是群本身）时照常答疑，按群计作提问者；以别的频道身份发的消息和其他 Bot 的消息不处理。
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
	if texts := tg.texts(); len(texts) != 1 || texts[0] != "在设置的「输入方案」里选五笔。" || limiter.counts["telegram-bot-member|tg:-1001234567890/tg:-1001234567890"] != 1 {
		t.Fatal(texts, limiter.counts)
	}
}
