package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// telegramPrivateJSON 是用户 user 私聊机器人的一条消息。
func telegramPrivateJSON(update, user int64, text string) string {
	raw, _ := json.Marshal(map[string]any{"update_id": update, "message": map[string]any{
		"message_id": update * 10, "from": map[string]any{"id": user, "is_bot": false}, "chat": map[string]any{"id": user, "type": "private"}, "text": text,
	}})
	return string(raw)
}

// 私聊机器人不用 @ 就是提问，所有私聊共用 tg:private 的群额度、每人各自计数；/start 当成帮助；私聊里只有 telegram_bot.admins 是管理员，不会因为 chat 就是自己而被当成匿名管理员。
func TestTelegramBotPrivateChat(t *testing.T) {
	s, fake, limiter, api := telegramAPIServer(t)
	s.tg.config.Admins = []int64{99}
	post := func(body string) {
		t.Helper()
		if w := telegramPost(s, testTelegramSecret, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		s.qqWorkers.Wait()
	}
	post(telegramPrivateJSON(81, 42, "五笔怎么切换"))
	if texts := api.texts(); len(texts) != 1 || texts[0] != "在设置的「输入方案」里选五笔。" || api.messages[0]["chat_id"] != float64(42) || strings.Join(fake.chatCalls, ",") != "select,answer" {
		t.Fatal(texts, api.messages, fake.chatCalls)
	}
	if limiter.counts["qq-bot-member|tg:private/tg:42"] != 1 || limiter.counts["qq-bot-group|tg:private"] != 1 {
		t.Fatal(limiter.counts)
	}
	post(telegramPrivateJSON(82, 42, "/start"))
	post(telegramPrivateJSON(83, 42, "/help@msime_bot"))
	post(telegramPrivateJSON(84, 42, "我的ID"))
	post(telegramPrivateJSON(85, 99, "我的ID"))
	texts := api.texts()
	if texts[1] != telegramHelpText || texts[2] != telegramHelpText || texts[3] != "你的 ID 是 tg:42，机器人把你当作普通成员。" || texts[4] != "你的 ID 是 tg:99，机器人把你当作管理员。" {
		t.Fatal(texts)
	}
	if api.roleCalls != 0 {
		t.Fatal("私聊里不该查群身份", api.roleCalls)
	}
	// Bot 发来的私聊不处理。
	post(strings.Replace(telegramPrivateJSON(86, 42, "在吗"), `"is_bot":false`, `"is_bot":true`, 1))
	if len(api.messages) != 5 {
		t.Fatal(api.texts())
	}
	if !strings.Contains(telegramHelpText, "私聊我的话不用 @") {
		t.Fatal(telegramHelpText)
	}
}

// 群里发 /bind 时绑定码私聊发给本人，群里只说已私聊；没私聊过机器人或是匿名管理员时，群里提示先私聊，码不出现在群里。私聊里发 /bind 直接回码。QQ 上照旧在群里回码。
func TestTelegramBotBindCodePrivately(t *testing.T) {
	s, _, _, api := telegramAPIServer(t)
	typing := &memoryTyping{links: map[string]bool{}}
	s.qqTyping = typing
	// codeMessages 是发出去的消息里带着绑定码的条数。
	codeMessages := func() int {
		n := 0
		for _, m := range api.messages {
			if strings.Contains(m["text"].(string), "12345678") {
				n++
			}
		}
		return n
	}
	telegramDeliver(t, s, 91, "@msime_bot /bind", nil)
	texts := api.texts()
	if len(texts) != 2 || !strings.Contains(texts[0], "绑定码 12345678，3 分钟内有效") || api.messages[0]["chat_id"] != float64(42) || texts[1] != qqTypingPrivateSent || api.messages[1]["chat_id"] != float64(testTelegramGroup) {
		t.Fatal(texts, api.messages)
	}
	if typing.codes["tg:-1001234567890/tg:42"] != 1 {
		t.Fatal("码要按群里的身份生成", typing.codes)
	}
	// 没私聊过机器人：群里提示先私聊，码不出现在任何地方。
	api.mu.Lock()
	api.blocked = map[int64]bool{42: true}
	api.mu.Unlock()
	telegramDeliver(t, s, 92, "@msime_bot 绑定", nil)
	if last := api.texts()[len(api.messages)-1]; last != qqTypingPrivateFirst || codeMessages() != 1 {
		t.Fatal(api.texts())
	}
	// 匿名管理员以群的身份发言，私聊不到：不能把码「私聊」发到群里。
	raw, _ := json.Marshal(map[string]any{"update_id": 93, "message": map[string]any{
		"message_id": 930, "from": map[string]any{"id": 1087968824, "is_bot": true}, "sender_chat": map[string]any{"id": testTelegramGroup, "type": "supergroup"},
		"chat": map[string]any{"id": testTelegramGroup, "type": "supergroup"}, "text": "@msime_bot /bind",
	}})
	if w := telegramPost(s, testTelegramSecret, string(raw)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.qqWorkers.Wait()
	if last := api.texts()[len(api.messages)-1]; last != qqTypingPrivateFirst || codeMessages() != 1 {
		t.Fatal(api.texts())
	}
	// 私聊里发 /bind：直接回码，按私聊的身份生成。
	api.mu.Lock()
	api.blocked = nil
	api.mu.Unlock()
	if w := telegramPost(s, testTelegramSecret, telegramPrivateJSON(94, 42, "/bind")); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.qqWorkers.Wait()
	if last := api.texts()[len(api.messages)-1]; !strings.Contains(last, "绑定码 12345678") || api.messages[len(api.messages)-1]["chat_id"] != float64(42) || typing.codes["tg:private/tg:42"] != 1 {
		t.Fatal(api.texts(), typing.codes)
	}
}
