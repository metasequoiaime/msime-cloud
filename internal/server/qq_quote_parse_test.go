package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func previousQQQuoteOf(raw json.RawMessage, m qqGroupMessage) qqQuote {
	var fields struct {
		MessageType      json.RawMessage `json:"message_type"`
		MsgElements      json.RawMessage `json:"msg_elements"`
		MessageReference json.RawMessage `json:"message_reference"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return qqQuote{}
	}
	var q qqQuote
	var elements []struct {
		Content     string         `json:"content"`
		Attachments []qqAttachment `json:"attachments"`
	}
	if json.Unmarshal(fields.MsgElements, &elements) == nil {
		own := map[string]bool{}
		for _, a := range m.Attachments {
			own[a.URL] = true
		}
		var texts []string
		for _, e := range elements {
			if text := qqFeedbackText(e.Content); text != "" && text != qqFeedbackText(m.Content) {
				texts = append(texts, text)
			}
			for _, a := range e.Attachments {
				if !own[a.URL] && qqHasImages([]qqAttachment{a}) {
					q.Images = append(q.Images, a)
				}
			}
		}
		q.Text = qqTruncate(strings.Join(texts, "\n"), qqQuoteRunes)
	}
	var messageType int
	q.Referenced = q.Text != "" || len(q.Images) > 0 || json.Unmarshal(fields.MessageType, &messageType) == nil && messageType == qqQuoteMessageType || strings.HasPrefix(string(fields.MessageReference), "{")
	return q
}

func TestQQQuoteOfMatchesPreviousForManyElements(t *testing.T) {
	m := qqGroupMessage{Content: "<@!bot> 这是什么 <qqbot-at-user id=\"1\" />", Attachments: []qqAttachment{{URL: "https://cdn/own.png", ContentType: "image/png"}}}
	elements := make([]map[string]any, 256)
	for i := range elements {
		elements[i] = map[string]any{
			"content":     "<qqbot-at-user id=\"2\" />引用文字 " + string(rune('a'+i%26)),
			"attachments": []map[string]string{{"url": "https://cdn/q" + string(rune('a'+i%26)) + ".png", "content_type": "image/png"}},
		}
	}
	raw, err := json.Marshal(map[string]any{"message_type": qqQuoteMessageType, "msg_elements": elements, "message_reference": map[string]string{"message_id": "quoted"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := qqQuoteOf(raw, m), previousQQQuoteOf(raw, m); got.Text != want.Text || got.Referenced != want.Referenced || len(got.Images) != len(want.Images) {
		t.Fatalf("引用解析不兼容：结果=%+v，期望=%+v", got, want)
	}
}

func TestQQQuoteOfBoundsRepeatedMessageCleanup(t *testing.T) {
	m := qqGroupMessage{Content: "<@!bot> 这是什么 <qqbot-at-user id=\"1\" />"}
	elements := make([]map[string]string, 256)
	for i := range elements {
		elements[i] = map[string]string{"content": "引用文字 " + string(rune('a'+i%26))}
	}
	raw, err := json.Marshal(map[string]any{"message_type": qqQuoteMessageType, "msg_elements": elements})
	if err != nil {
		t.Fatal(err)
	}
	var got qqQuote
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			got = qqQuoteOf(raw, m)
		}
	})
	if !got.Referenced || !strings.HasPrefix(got.Text, "引用文字") {
		t.Fatalf("引用解析结果错误：%+v", got)
	}
	allocated := result.AllocedBytesPerOp()
	if !raceEnabled && allocated > 60<<10 {
		t.Fatalf("多元素引用解析分配 %d 字节，超过 60 KiB 预算", allocated)
	}
}

func BenchmarkQQQuoteOf(b *testing.B) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		m    qqGroupMessage
	}{
		{"single", json.RawMessage(`{"message_type":103,"msg_elements":[{"content":"候选框在哪"} ]}`), qqGroupMessage{Content: "这个怎么弄"}},
		{"many_markup", func() json.RawMessage {
			elements := make([]map[string]string, 128)
			for i := range elements {
				elements[i] = map[string]string{"content": "引用文字 " + string(rune('a'+i%26))}
			}
			raw, _ := json.Marshal(map[string]any{"message_type": qqQuoteMessageType, "msg_elements": elements})
			return raw
		}(), qqGroupMessage{Content: "<@!bot> 这是什么 <qqbot-at-user id=\"1\" />"}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = qqQuoteOf(tc.raw, tc.m)
			}
		})
	}
}
