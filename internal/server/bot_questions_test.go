package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestBotQuestionModelConfig(t *testing.T) {
	t.Setenv("TEST_QQ_SECRET", qqTestSecret)
	for _, platform := range []string{botPlatformQQ, botPlatformTelegram} {
		c := qqValidConfig()
		c.QQBot.AutoReply = platform == botPlatformQQ
		c.TelegramBot.AutoReply = platform == botPlatformTelegram
		if err := c.QQBot.validate(&c); err != nil || c.QQBot.QuestionModel != defaultBotQuestionModel {
			t.Fatal(platform, err)
		}
		if err := c.QQBot.validate(&c); err != nil {
			t.Fatal("重复校验失败", err)
		}
	}
	for _, model := range []string{"has space", strings.Repeat("x", 201)} {
		c := qqValidConfig()
		c.QQBot.QuestionModel = model
		if err := c.QQBot.validate(&c); err == nil {
			t.Fatal("接受了错误模型配置")
		}
	}
	c := qqValidConfig()
	c.QQBot.QuestionModel = "custom-classifier"
	if err := c.QQBot.validate(&c); err != nil || c.QQBot.QuestionModel != "custom-classifier" {
		t.Fatal("自定义模型失败", err)
	}
	c = qqValidConfig()
	if err := c.QQBot.validate(&c); err != nil || c.QQBot.QuestionModel != "" {
		t.Fatal("关闭时不应设置分类模型", err)
	}
	if err := (&QQBotConfig{AutoReply: true}).validate(&Config{}); err == nil {
		t.Fatal("没有 app_id 时接受了自动答疑")
	}
	if err := (&QQBotConfig{QuestionModel: defaultBotQuestionModel}).validate(&Config{}); err == nil {
		t.Fatal("没有 app_id 时接受了分类模型")
	}
	if err := (&TelegramBotConfig{AutoReply: true}).validate(&Config{}); err == nil {
		t.Fatal("没有 chats 时接受了自动答疑")
	}
}

func TestBotQuestionCandidate(t *testing.T) {
	for _, text := range []string{"五笔怎么切换", "求教双拼设置", "How do I enable Wubi", "打不了字了，求助"} {
		if !botQuestionCandidate(text) {
			t.Fatal("漏掉了候选消息", text)
		}
	}
	for _, text := range []string{"", "  ", "？", "😀", "<@!102030405>", "/issue 候选框有问题", strings.Repeat("长", qqBotMaxRunes+1)} {
		if botQuestionCandidate(text) {
			t.Fatal("不应送入分类", text[:min(len(text), 80)])
		}
	}
}

// installBotClassifier 用本地接口代替 EveryAPI，核对复用 chat 地址和密钥、只发送消息文字。
func installBotClassifier(t *testing.T, s *Server, response string, status int) *int {
	t.Helper()
	calls := new(int)
	original := s.client
	s.qq.config.QuestionModel = defaultBotQuestionModel
	client := *original
	client.Transport = qqTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body == nil {
			return original.Do(r)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var request chatRequest
		if json.Unmarshal(body, &request) != nil || request.Model != defaultBotQuestionModel {
			return original.Do(r)
		}
		*calls++
		if r.URL.String() != s.config.Chat.URL || r.Header.Get("Authorization") != "Bearer "+s.config.Chat.token || s.config.Chat.token == "" || request.MaxTokens != botQuestionMaxTokens || request.ResponseFormat == nil || request.ResponseFormat.Type != "json_object" || request.Thinking == nil || request.Thinking.Type != "disabled" || len(request.Messages) != 3 {
			t.Error("分类没有沿用 chat 端点、凭据或费用限制")
		} else if request.Messages[0].Content != botQuestionPrompt || request.Messages[1].Role != "user" || request.Messages[2].Content != jsonObjectInstruction || strings.Contains(request.Messages[1].Content, "GROUP1") || strings.Contains(request.Messages[1].Content, "MEMBER1") {
			t.Error("分类器收到了身份或非文字状态")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	s.client = &client
	return calls
}

const botClassifierYes = `{"choices":[{"message":{"content":"{\"question\":true,\"relevant\":true}"},"finish_reason":"stop"}]}`

func botClassifierResponse(content string) string {
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}, "finish_reason": "stop"}}})
	return string(body)
}

func TestBotClassifierModelSelection(t *testing.T) {
	for _, model := range []string{"", "custom-classifier"} {
		s, _, _ := qqServer(t)
		s.qq.config.QuestionModel = model
		s.client = &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
			var request chatRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Fatal("请求不是 JSON")
			}
			want := model
			if want == "" {
				want = defaultBotQuestionModel
			}
			if request.Model != want || (request.Thinking != nil) != strings.HasPrefix(want, "doubao-") {
				t.Error("没有使用配置的模型或附带了不支持的思考参数", request.Model)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(botClassifierYes)), Header: make(http.Header)}, nil
		})}
		if yes, err := s.botClassifyQuestion(context.Background(), "五笔怎么切换"); err != nil || !yes {
			t.Fatal(yes, err)
		}
	}
}

func TestBotClassifierRejectsInvalidResponses(t *testing.T) {
	for _, response := range []string{
		`{}`, `not json`, botClassifierResponse(`{"question":true}`),
		botClassifierResponse(`{"question":null,"relevant":true}`),
		botClassifierResponse(`{"question":"true","relevant":true}`),
		botClassifierResponse(`{"question":1,"relevant":true}`),
		botClassifierResponse("```json\n{\"question\":true,\"relevant\":true}\n```"),
		strings.Replace(botClassifierYes, `"stop"`, `"length"`, 1),
		botClassifierYes + ` trailing`, strings.Repeat("x", 16<<10+1),
	} {
		s, _, _ := qqServer(t)
		installBotClassifier(t, s, response, http.StatusOK)
		if _, err := s.botClassifyQuestion(context.Background(), "五笔怎么切换"); err == nil {
			t.Fatal("接受了错误响应", response[:min(len(response), 100)])
		}
	}
	for _, status := range []int{401, 429, 500} {
		s, _, _ := qqServer(t)
		installBotClassifier(t, s, `private-upstream-body`, status)
		if _, err := s.botClassifyQuestion(context.Background(), "五笔怎么切换"); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("错误未被清洗", status, err)
		}
	}
	s, _, _ := qqServer(t)
	s.client = &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive provider error") })}
	if _, err := s.botClassifyQuestion(context.Background(), "五笔怎么切换"); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("网络错误未被清洗", err)
	}
	s.config.Chat.URL = "://invalid"
	if _, err := s.botClassifyQuestion(context.Background(), "五笔怎么切换"); err == nil {
		t.Fatal("接受了错误端点")
	}
}

// 两个平台共用同一条分类和答疑流程；不确定或不相关时，不调用生成模型、不占答疑额度。
func TestBotAutomaticReplies(t *testing.T) {
	for _, platform := range []string{botPlatformQQ, botPlatformTelegram} {
		t.Run(platform, func(t *testing.T) {
			s, fake, limiter, tg := telegramServer(t)
			s.qq.config.AutoReply, s.tg.config.AutoReply = true, true
			calls := installBotClassifier(t, s, botClassifierYes, 200)
			deliver := func(id int, text string) {
				if platform == botPlatformTelegram {
					telegramDeliver(t, s, int64(id), text, nil)
				} else {
					w := qqPost(t, s, strings.Replace(qqMessage("auto-"+strconv.Itoa(id), text), "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"d":0`) {
						t.Fatal(w.Code, w.Body.String())
					}
					s.qqWorkers.Wait()
				}
			}
			texts := func() []string {
				if platform == botPlatformTelegram {
					return tg.texts()
				}
				return fake.replyTexts()
			}
			deliver(1, "五笔怎么切换")
			if *calls != 1 || len(texts()) != 1 || texts()[0] != "在设置的「输入方案」里选五笔。" || strings.Join(fake.chatCalls, ",") != "select,answer" {
				t.Fatal(*calls, texts(), fake.chatCalls)
			}
			// 同一个 QQ 消息或 Telegram update 重推时，连分类也不重复。
			deliver(1, "五笔怎么切换")
			if *calls != 1 || len(texts()) != 1 {
				t.Fatal("重复处理", *calls, texts())
			}
			for i, response := range []string{
				botClassifierResponse(`{"question":false,"relevant":true}`),
				botClassifierResponse(`{"question":true,"relevant":false}`),
				`{}`,
			} {
				installBotClassifier(t, s, response, 200)
				deliver(i+2, "这是真的吗")
			}
			if len(texts()) != 1 || len(fake.chatCalls) != 2 {
				t.Fatal("对未确认的提问作答", texts(), fake.chatCalls)
			}
			group, member := "GROUP1", "MEMBER1"
			if platform == botPlatformTelegram {
				group, member = "tg:-1001234567890", "tg:42"
			}
			// 分类额度用完时保持安静；显式 @ 仍可使用原来的答疑额度。
			installBotClassifier(t, s, botClassifierYes, 200)
			limiter.counts["bot-classify-group|"+group] = botClassifyGroupDaily
			deliver(8, "五笔怎么切换")
			if len(texts()) != 1 {
				t.Fatal(texts())
			}
			if platform == botPlatformQQ {
				qqDeliver(t, s, "explicit", "五笔怎么切换")
			} else {
				telegramDeliver(t, s, 9, "@msime_bot 五笔怎么切换", nil)
			}
			if len(texts()) != 2 {
				t.Fatal("显式提问被分类额度阻止", texts())
			}
			// 原有答疑额度用完时自动提问不刷限流通知。
			delete(limiter.counts, "bot-classify-group|"+group)
			limiter.counts["qq-bot-member|"+group+"/"+member] = qqBotMemberDaily
			limiter.counts["qq-bot-admin|"+member] = qqBotAdminDaily
			deliver(10, "五笔怎么切换")
			if len(texts()) != 2 {
				t.Fatal("自动答疑刷限流通知", texts())
			}
		})
	}
}

func TestBotAutomaticFilters(t *testing.T) {
	s, fake, _, tg := telegramServer(t)
	calls := installBotClassifier(t, s, botClassifierYes, 200)
	// 默认关闭时普通群消息不送入分类。
	telegramDeliver(t, s, 1, "五笔怎么切换", nil)
	qqPost(t, s, strings.Replace(qqMessage("off", "五笔怎么切换"), "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
	s.qqWorkers.Wait()
	s.qq.config.AutoReply, s.tg.config.AutoReply = true, true
	for i, text := range []string{"😀", "？", "/issue 候选框有问题", "@someone 五笔怎么切换", strings.Repeat("长", qqBotMaxRunes+1)} {
		if i != 2 {
			telegramDeliver(t, s, int64(i+2), text, nil)
		}
		qqPost(t, s, strings.Replace(qqMessage(strconv.Itoa(i), text), "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
	}
	telegramDeliver(t, s, 10, "五笔怎么切换", map[string]any{"from": map[string]any{"id": 99}, "text": "别人的发言"})
	bot := strings.Replace(qqMessage("from-bot", "五笔怎么切换"), `"member_openid":"MEMBER1"`, `"member_openid":"MEMBER1","bot":true`, 1)
	qqPost(t, s, strings.Replace(bot, "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
	// 即使正文没有 @ 标记，事件中提到了其他人也不插话或提前占用 @ 事件的消息编号。
	mentioned := strings.Replace(qqMessage("other-mention", "五笔怎么切换"), `"group_openid":"GROUP1"`, `"mentions":[{"id":"OTHER","is_you":false}],"group_openid":"GROUP1"`, 1)
	qqPost(t, s, strings.Replace(mentioned, "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
	s.qqWorkers.Wait()
	if *calls != 0 || len(fake.chatCalls) != 0 || len(fake.replies) != 0 || len(tg.messages) != 0 {
		t.Fatal(*calls, fake.chatCalls, fake.replies, tg.texts())
	}
}

func TestBotAutomaticNeverExecutesCommands(t *testing.T) {
	s, fake, _, _ := telegramServer(t)
	s.qq.config.AutoReply = true
	installBotClassifier(t, s, botClassifierYes, 200)
	for i, text := range []string{"我的ID", "提issue 候选框跑到左上角了", "提文档 设置里找不到五笔"} {
		qqPost(t, s, strings.Replace(qqMessage("auto-command-"+strconv.Itoa(i), text), "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
		s.qqWorkers.Wait()
	}
	if strings.Join(fake.chatCalls, ",") != "select,answer,select,answer,select,answer" || len(fake.created) != 0 || strings.Contains(strings.Join(fake.replyTexts(), ""), "MEMBER1") {
		t.Fatal("分类错误触发了命令或提交反馈", fake.chatCalls, fake.replyTexts(), fake.created)
	}
	// 全量事件里的明确 @ 绕过分类，开启自动答疑与否都可用命令；两个事件共享消息去重。
	for i, content := range []string{"<@!102030405> 我的ID", "<@102030405> 我的ID"} {
		id := "explicit-full-" + strconv.Itoa(i)
		s.qq.config.AutoReply = i == 0
		qqPost(t, s, strings.Replace(qqMessage(id, content), "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1))
		s.qqWorkers.Wait()
		if got := fake.replyTexts(); !strings.HasPrefix(got[len(got)-1], "你的 ID 是 MEMBER1") {
			t.Fatal(got)
		}
		before := len(fake.replyTexts())
		qqDeliver(t, s, id, content)
		if len(fake.replyTexts()) != before {
			t.Fatal("两个事件重复回复")
		}
	}
	// 机器人 OpenID 不同于 AppID 时，以平台的 is_you 标记认出明确呼叫。
	raw := strings.Replace(qqMessage("self-mention", "<@BOT_OPENID> 我的ID"), "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", 1)
	raw = strings.Replace(raw, `"group_openid":"GROUP1"`, `"mentions":[{"id":"BOT_OPENID","is_you":true}],"group_openid":"GROUP1"`, 1)
	qqPost(t, s, raw)
	s.qqWorkers.Wait()
	if got := fake.replyTexts(); !strings.HasPrefix(got[len(got)-1], "你的 ID 是 MEMBER1") {
		t.Fatal(got)
	}
}

func TestBotClassificationBudgetsAndFailures(t *testing.T) {
	for _, scope := range []string{"bot-classify-member", "bot-classify-group", "bot-classify-minute"} {
		t.Run(scope, func(t *testing.T) {
			s, _, limiter := qqServer(t)
			calls := installBotClassifier(t, s, botClassifierYes, 200)
			limiter.err = map[string]error{scope: errors.New("database unavailable")}
			m := botMessage{Text: "五笔怎么切换", Group: "GROUP1", Member: "MEMBER1"}
			if s.botShouldReply(context.Background(), m) || *calls != 0 {
				t.Fatal("数据库失败后仍分类", *calls)
			}
			limiter.err = nil
			subject, limit := "GROUP1", botClassifyGroupDaily
			if scope == "bot-classify-member" {
				subject, limit = m.subject(), botClassifyMemberDaily
			}
			if scope == "bot-classify-minute" {
				limit = botClassifyGroupMinute
			}
			limiter.counts = map[string]int{scope + "|" + subject: limit}
			if s.botShouldReply(context.Background(), m) || *calls != 0 {
				t.Fatal("超额后仍分类", *calls)
			}
		})
	}
	for _, classificationFails := range []bool{false, true} {
		s, fake, _, tg := telegramServer(t)
		s.tg.config.AutoReply = true
		status := 200
		if classificationFails {
			status = 503
		} else {
			fake.chatStatus = 503
		}
		calls := installBotClassifier(t, s, botClassifierYes, status)
		telegramDeliver(t, s, 1, "五笔怎么切换", nil)
		if *calls != 1 || len(tg.messages) != 0 {
			t.Fatal("失败时插话或重试", *calls, tg.texts())
		}
	}
}
