package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestQQGroupMessageCarriesQuoteFieldsFromInitialDecode(t *testing.T) {
	raw := json.RawMessage(`{"id":"m1","content":"question","message_type":103,"msg_elements":[{"content":"quoted"}],"message_reference":{"message_id":"ref"}}`)
	var message qqGroupMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	if len(message.MessageType) == 0 || len(message.MsgElements) == 0 || len(message.MessageReference) == 0 {
		t.Fatal("首次事件解码没有保留引用字段")
	}
	want := qqQuoteOf(raw, message)
	got := qqQuoteFromFields(qqQuoteEventFields{MessageType: message.MessageType, MsgElements: message.MsgElements, MessageReference: message.MessageReference}, message)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("复用首次解码的引用结果改变：得到 %+v，期望 %+v", got, want)
	}
}

func TestQQQuoteOfSkipsElementsDecodeForMissingOrScalar(t *testing.T) {
	m := qqGroupMessage{Content: "synthetic-question"}
	for _, body := range []string{
		`{}`,
		`{"msg_elements":null}`,
		`{"msg_elements":false,"message_type":0}`,
		`{"msg_elements":1,"message_reference":"synthetic-reference"}`,
	} {
		t.Run(body, func(t *testing.T) {
			raw := json.RawMessage(body)
			var fields qqQuoteEventFields
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			allocations := testing.AllocsPerRun(100, func() { _ = qqQuoteFromFields(fields, m) })
			if allocations > 8 {
				t.Fatalf("缺失或标量 msg_elements 仍分配 %.0f 次，预算 8 次", allocations)
			}
		})
	}
}

func BenchmarkQQQuoteOfNonArrayElements(b *testing.B) {
	for _, tc := range []struct{ name, body string }{
		{"missing", `{}`},
		{"null", `{"msg_elements":null}`},
		{"scalar", `{"msg_elements":false,"message_type":0}`},
		{"array", `{"msg_elements":[{"content":"synthetic-quote"}]}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			raw := json.RawMessage(tc.body)
			b.ReportAllocs()
			for b.Loop() {
				_ = qqQuoteOf(raw, qqGroupMessage{Content: "synthetic-question"})
			}
		})
	}
}

func BenchmarkQQQuoteReuseAfterInitialDecode(b *testing.B) {
	elements := make([]map[string]string, 128)
	for i := range elements {
		elements[i] = map[string]string{"content": "引用文字 " + string(rune('a'+i%26))}
	}
	raw, _ := json.Marshal(map[string]any{"message_type": qqQuoteMessageType, "msg_elements": elements})
	m := qqGroupMessage{Content: "<@!bot> 这是什么 <qqbot-at-user id=\"1\" />"}
	var decoded qqGroupMessage
	if json.Unmarshal(raw, &decoded) != nil {
		b.Fatal("合成消息解码失败")
	}
	fields := qqQuoteEventFields{MessageType: decoded.MessageType, MsgElements: decoded.MsgElements, MessageReference: decoded.MessageReference}
	for _, tc := range []struct {
		name string
		fn   func() qqQuote
	}{
		{"rescan", func() qqQuote { return qqQuoteOf(raw, m) }},
		{"reuse", func() qqQuote { return qqQuoteFromFields(fields, m) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got := tc.fn()
				if got.Text == "" || !strings.Contains(got.Text, "引用文字") {
					b.Fatal("引用解析结果错误")
				}
			}
		})
	}
}

func BenchmarkQQQuoteFullMessagePath(b *testing.B) {
	elements := make([]map[string]string, 128)
	for i := range elements {
		elements[i] = map[string]string{"content": "引用文字 " + string(rune('a'+i%26))}
	}
	raw, _ := json.Marshal(map[string]any{"id": "m1", "content": "<@!bot> 这是什么 <qqbot-at-user id=\"1\" />", "group_openid": "GROUP1", "author": map[string]string{"member_openid": "MEMBER1"}, "message_type": qqQuoteMessageType, "msg_elements": elements})
	type previousMessage struct {
		ID, Content, GroupOpenID string
		Author                   struct {
			MemberOpenID string `json:"member_openid"`
		} `json:"author"`
		Attachments []qqAttachment `json:"attachments"`
	}
	for _, tc := range []struct {
		name string
		fn   func() qqQuote
	}{
		{"previous", func() qqQuote {
			var m previousMessage
			_ = json.Unmarshal(raw, &m)
			return qqQuoteOf(raw, qqGroupMessage{Content: m.Content})
		}},
		{"reused", func() qqQuote {
			var m qqGroupMessage
			_ = json.Unmarshal(raw, &m)
			return qqQuoteFromFields(qqQuoteEventFields{MessageType: m.MessageType, MsgElements: m.MsgElements, MessageReference: m.MessageReference}, m)
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := tc.fn(); got.Text == "" {
					b.Fatal("引用解析结果为空")
				}
			}
		})
	}
}
