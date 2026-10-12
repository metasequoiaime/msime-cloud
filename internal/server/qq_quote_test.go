package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// qqQuotedMessage 是一条带 extra 字段的群 @ 事件，用来模拟引用消息。
func qqQuotedMessage(id, content string, extra map[string]any) string {
	d := map[string]any{"id": id, "content": content, "group_openid": "GROUP1", "author": map[string]string{"member_openid": "MEMBER1"}}
	for k, v := range extra {
		d[k] = v
	}
	event, _ := json.Marshal(map[string]any{"op": 0, "t": "GROUP_AT_MESSAGE_CREATE", "d": d})
	return string(event)
}

// 引用的消息：msg_elements 里的文字和图片算引用，和消息自己相同的不算；message_type=103 或 message_reference 只说明引用了；字段类型对不上时当没有引用。
func TestQQQuoteOf(t *testing.T) {
	m := qqGroupMessage{Content: "<@!bot> 这括号里的AD什么意思", Attachments: []qqAttachment{{URL: "https://cdn/own.png", ContentType: "image/png"}}}
	cases := []struct {
		name, d string
		want    qqQuote
	}{
		{"elements", `{"message_type":103,"msg_elements":[{"content":"<@!bot> 候选后面的字母","attachments":[{"url":"https://cdn/q.png","content_type":"image/png"},{"url":"https://cdn/v.mp4","content_type":"video/mp4"}]}]}`,
			qqQuote{Text: "候选后面的字母", Images: []qqAttachment{{URL: "https://cdn/q.png", ContentType: "image/png"}}, Referenced: true}},
		{"own content and attachments only", `{"msg_elements":[{"content":"这括号里的AD什么意思","attachments":[{"url":"https://cdn/own.png","content_type":"image/png"}]}]}`, qqQuote{}},
		{"type only", `{"message_type":103}`, qqQuote{Referenced: true}},
		{"reference only", `{"message_reference":{"message_id":"ROBOT1.0_x"}}`, qqQuote{Referenced: true}},
		{"null reference", `{"message_reference":null,"message_type":0}`, qqQuote{}},
		{"unexpected types", `{"message_type":"103","msg_elements":{"content":"x"}}`, qqQuote{}},
	}
	for _, c := range cases {
		got := qqQuoteOf(json.RawMessage(c.d), m)
		if got.Text != c.want.Text || got.Referenced != c.want.Referenced || len(got.Images) != len(c.want.Images) || len(got.Images) > 0 && got.Images[0] != c.want.Images[0] {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if got := qqQuoteOf(json.RawMessage(`{"msg_elements":[{"content":"`+strings.Repeat("长", qqQuoteRunes+50)+`"}]}`), m); len([]rune(got.Text)) > qqQuoteRunes+1 {
		t.Fatal(len([]rune(got.Text)))
	}
}

// 引用了截图再问：截图交给看图模型，描述接在问题后面；引用的文字接在问题后面；只 @ 不写话时引用的话就是问题；看不到引用内容时注明；命令不带引用的文字；字段类型对不上的消息照常处理。
func TestQQBotQuotedMessages(t *testing.T) {
	s, fake, _ := qqServer(t)
	qqTestMediaHosts(s)
	s.qq.config.Admins = nil
	lastBody := func() string { return fake.chatBodies[len(fake.chatBodies)-1] }
	deliver := func(id, content string, extra map[string]any) {
		t.Helper()
		qqPost(t, s, qqQuotedMessage(id, content, extra))
		s.qqWorkers.Wait()
	}
	shot := []map[string]string{{"url": s.config.Chat.URL + "/media/shot.png", "content_type": "image/png"}}
	deliver("m-q-shot", "这括号里的AD什么意思", map[string]any{"message_type": 103, "msg_elements": []map[string]any{{"content": "", "attachments": shot}}})
	if len(fake.visionCalls) != 1 || !strings.HasPrefix(lastBody(), "这括号里的AD什么意思\n（群友附了截图，截图内容：") {
		t.Fatal(fake.visionCalls, fake.chatBodies)
	}
	deliver("m-q-text", "这个怎么弄", map[string]any{"message_type": 103, "msg_elements": []map[string]any{{"content": "候选框跑到左上角了"}}})
	if lastBody() != "这个怎么弄\n（群友引用的消息：「候选框跑到左上角了」）" {
		t.Fatalf("%q", lastBody())
	}
	deliver("m-q-empty", "", map[string]any{"msg_elements": []map[string]any{{"content": "五笔怎么切换"}}})
	if lastBody() != "五笔怎么切换" {
		t.Fatalf("%q", lastBody())
	}
	deliver("m-q-unseen", "这括号里的AD什么意思", map[string]any{"message_type": 103})
	if lastBody() != "这括号里的AD什么意思"+qqQuoteUnseenNote {
		t.Fatalf("%q", lastBody())
	}
	before := len(fake.chatBodies)
	fake.releases = map[string]string{}
	deliver("m-q-cmd", "版本", map[string]any{"msg_elements": []map[string]any{{"content": "候选框跑到左上角了"}}})
	if len(fake.chatBodies) != before {
		t.Fatal("command went to the model", fake.chatBodies[before:])
	}
	deliver("m-q-odd", "五笔怎么切换", map[string]any{"message_type": "quote", "msg_elements": "x", "message_reference": 5})
	if lastBody() != "五笔怎么切换" {
		t.Fatalf("%q", lastBody())
	}
}
