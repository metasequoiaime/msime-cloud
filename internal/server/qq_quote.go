package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// 引用的消息。群友常常引用（回复）一条带截图或问题的消息，再 @机器人 问「这个是什么意思」，机器人原来只收到这一句，看不到被引用的截图，只能瞎猜：曾把候选后面括号里的辅助码「(AD)」答成广告、公元、活动目录。开放平台的群 @ 事件里引用的内容放在哪个字段，文档没写清、生产上也还没见过：这里按已知的写法认 msg_elements（被引用消息的文字和附件），message_reference（对象）和 message_type=103 只说明引用了。这几个字段单独解析，类型对不上就当没有引用，不影响 qqGroupMessage 本身的解析（字段类型猜错会让整条消息被丢掉）；probeEventFields 会记下带这些字段的事件长什么样，确认之后再按实际字段调整。

// qqQuoteFields 是可能带着引用信息的字段，probeEventFields 记它们下一层的字段名。
var qqQuoteFields = []string{"message_reference", "msg_elements", "message_scene"}

const (
	// qqQuoteMessageType 是开放平台表示引用消息的 message_type。
	qqQuoteMessageType = 103
	// qqQuoteRunes 是接到问题后面的引用文字最多多少字。
	qqQuoteRunes = 500
	// qqQuoteUnseenNote 接在问题后面：知道群友引用了消息、却拿不到内容时，让回答的模型先按输入法里最可能的意思答，再请他把截图和问题放在同一条消息里。
	qqQuoteUnseenNote = "\n（群友引用了一条消息来问，但机器人看不到引用的内容；回答时先按输入法里最可能的意思说，再请他把截图或原话和问题放在同一条消息里 @机器人）"
)

// qqQuote 是群友引用的那条消息：Text 是去掉 @ 标记的文字，Images 是其中的图片；Referenced 表示事件说明了这是一条引用消息，即使拿不到内容。
type qqQuote struct {
	Text       string
	Images     []qqAttachment
	Referenced bool
}

type qqQuoteEventFields struct {
	MessageType      json.RawMessage `json:"message_type"`
	MsgElements      json.RawMessage `json:"msg_elements"`
	MessageReference json.RawMessage `json:"message_reference"`
}

// qqQuoteArrayElementCount 统计 JSON 数组的顶层元素，为反序列化预留切片容量。
func qqQuoteArrayElementCount(raw []byte) int {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' {
		return 0
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n' {
			continue
		}
		if raw[i] == ']' {
			return 0
		}
		break
	}

	depth := 1
	count := 1
	inString := false
	escaped := false
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		switch c {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return count
			}
		case ',':
			if depth == 1 {
				count++
			}
		}
	}
	return 0
}

// qqQuoteOf 从事件 d 里取出引用的消息。msg_elements 里和这条消息自己相同的文字、附件不算引用：不确定开放平台会不会把消息本身也放进去。
func qqQuoteOf(raw json.RawMessage, m qqGroupMessage) qqQuote {
	var fields qqQuoteEventFields
	if json.Unmarshal(raw, &fields) != nil {
		return qqQuote{}
	}
	return qqQuoteFromFields(fields, m)
}

func qqQuoteFromFields(fields qqQuoteEventFields, m qqGroupMessage) qqQuote {
	var q qqQuote
	elements := make([]struct {
		Content     string         `json:"content"`
		Attachments []qqAttachment `json:"attachments"`
	}, 0, qqQuoteArrayElementCount(fields.MsgElements))
	// 缺失、null 和标量值不可能解码成引用元素数组，直接跳过，避免为类型错误创建临时对象。
	if elementsRaw := bytes.TrimSpace(fields.MsgElements); len(elementsRaw) > 0 && elementsRaw[0] == '[' && json.Unmarshal(elementsRaw, &elements) == nil {
		var own map[string]bool
		if len(m.Attachments) > 0 {
			own = make(map[string]bool, len(m.Attachments))
			for _, a := range m.Attachments {
				own[a.URL] = true
			}
		}
		ownText := qqFeedbackText(m.Content)
		var texts []string
		// 前 500 个字符最多占 2000 字节，多保留一字节供 qqTruncate 判断省略号。
		// 字节边界可能切进后续字符，但不会切断最终保留的字符；达到预算后仍须检查附件。
		remaining := qqQuoteRunes*utf8.UTFMax + 1
		for _, e := range elements {
			if remaining > 0 {
				if text := qqFeedbackText(e.Content); text != "" && text != ownText {
					if len(texts) > 0 {
						remaining--
					}
					if len(text) > remaining {
						text = text[:remaining]
					}
					remaining -= len(text)
					texts = append(texts, text)
				}
			}
			for _, a := range e.Attachments {
				if !own[a.URL] && qqImageContentType(a.ContentType) {
					q.Images = append(q.Images, a)
				}
			}
		}
		q.Text = qqTruncate(strings.Join(texts, "\n"), qqQuoteRunes)
	}
	var messageType int
	q.Referenced = q.Text != "" || len(q.Images) > 0 || json.Unmarshal(fields.MessageType, &messageType) == nil && messageType == qqQuoteMessageType || bytes.HasPrefix(fields.MessageReference, []byte("{"))
	return q
}
