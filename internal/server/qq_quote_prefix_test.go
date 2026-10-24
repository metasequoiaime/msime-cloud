package server

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// previousQQQuoteFromFields 固定优化前的处理方式，核对文字截断和附件筛选语义。
func previousQQQuoteFromFields(fields qqQuoteEventFields, m qqGroupMessage) qqQuote {
	var q qqQuote
	var elements []struct {
		Content     string         `json:"content"`
		Attachments []qqAttachment `json:"attachments"`
	}
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
		for _, e := range elements {
			if text := qqFeedbackText(e.Content); text != "" && text != ownText {
				texts = append(texts, text)
			}
			for _, a := range e.Attachments {
				if !own[a.URL] && strings.HasPrefix(strings.ToLower(a.ContentType), "image/") {
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

func qqQuotePrefixFields(tb testing.TB, contents []string) qqQuoteEventFields {
	tb.Helper()
	elements := make([]map[string]any, len(contents))
	for i, content := range contents {
		elements[i] = map[string]any{"content": content}
	}
	if len(elements) > 0 {
		elements[len(elements)-1]["attachments"] = []qqAttachment{
			{URL: "https://example.test/own.png", ContentType: "image/png"},
			{URL: "https://example.test/quote.png", ContentType: "IMAGE/PNG"},
			{URL: "https://example.test/quote.mp4", ContentType: "video/mp4"},
		}
	}
	raw, err := json.Marshal(elements)
	if err != nil {
		tb.Fatal(err)
	}
	return qqQuoteEventFields{MsgElements: raw}
}

func TestQQQuotePrefixMatchesPrevious(t *testing.T) {
	m := qqGroupMessage{Content: "<@!bot> 自己的消息", Attachments: []qqAttachment{{URL: "https://example.test/own.png", ContentType: "image/png"}}}
	cases := [][]string{
		nil, {""}, {"自己的消息", " <@!bot> 自己的消息 ", "\u3000"},
		{"引用正文"}, {"<@!bot> 第一段", "\u3000第二段 😀 "},
		{strings.Repeat("字", 499)}, {strings.Repeat("字", 500)}, {strings.Repeat("字", 501)},
		{strings.Repeat("字", 499), "", "自己的消息", "<@!bot>", "尾"},
		{strings.Repeat("字", 500), "", "自己的消息", "<@!bot>"},
		{strings.Repeat("字", 500), "尾"},
		{strings.Repeat("字", 498), "😀", "尾"},
		{strings.Repeat("字", 499), "<@!bot>" + strings.Repeat("尾", 2000)},
		{strings.Repeat("😀", 500)}, {strings.Repeat("😀", 501)},
		{strings.Repeat("😀", 500), "自己的消息", "尾"},
		{strings.Repeat("😀", 499), "😀", "尾"},
		{strings.Repeat("😀", 499) + "字字", "😀"},
		{"\xff", strings.Repeat("😀", 500)},
	}
	random := rand.New(rand.NewSource(1069))
	units := []string{"字", "A", "😀", "\r\n", "\u3000", "\ufffd", "\xff", "<@!bot>", "<emoji:1>", "<未闭合"}
	for range 120 {
		parts := make([]string, 1+random.Intn(10))
		for i := range parts {
			var part strings.Builder
			for range random.Intn(300) {
				part.WriteString(units[random.Intn(len(units))])
			}
			parts[i] = part.String()
		}
		cases = append(cases, parts)
	}
	for i, contents := range cases {
		fields := qqQuotePrefixFields(t, contents)
		if got, want := qqQuoteFromFields(fields, m), previousQQQuoteFromFields(fields, m); !reflect.DeepEqual(got, want) {
			t.Fatalf("用例 %d 的引用结果改变：结果=%+v，期望=%+v", i, got, want)
		}
	}
	for _, raw := range []string{"null", "{}", `[{"content":1}]`, "[", `[{"content":"\ud800"}]`, "[{\"content\":\"\xff\"}]"} {
		fields := qqQuoteEventFields{MsgElements: json.RawMessage(raw), MessageType: json.RawMessage("103")}
		if got, want := qqQuoteFromFields(fields, m), previousQQQuoteFromFields(fields, m); !reflect.DeepEqual(got, want) {
			t.Fatalf("异常或替换字符输入的引用结果改变：输入=%q，结果=%+v，期望=%+v", raw, got, want)
		}
	}
}

func TestQQQuotePrefixKeepsLaterImages(t *testing.T) {
	fields := qqQuotePrefixFields(t, []string{strings.Repeat("😀", qqQuoteRunes+1), "自己的消息", " <@!bot> 尾部引用 "})
	m := qqGroupMessage{Content: "自己的消息", Attachments: []qqAttachment{{URL: "https://example.test/own.png", ContentType: "image/png"}}}
	got := qqQuoteFromFields(fields, m)
	if got.Text != strings.Repeat("😀", qqQuoteRunes)+"…" || !got.Referenced || len(got.Images) != 1 || got.Images[0].URL != "https://example.test/quote.png" {
		t.Fatalf("文字截断后仍应保留尾部图片、排除自己的附件与视频：%+v", got)
	}
}

func TestQQQuotePrefixAllocationBudget(t *testing.T) {
	contents := make([]string, 8)
	for i := range contents {
		contents[i] = strings.Repeat("引用文字", 500)
	}
	fields := qqQuotePrefixFields(t, contents)
	if len(fields.MsgElements) >= qqBotBodyBytes-1024 {
		t.Fatal("分配回归样本必须留出事件信封空间，保持在回调请求上限以内")
	}
	var got qqQuote
	const runs = 20
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		got = qqQuoteFromFields(fields, qqGroupMessage{})
	}
	runtime.ReadMemStats(&after)
	if want := previousQQQuoteFromFields(fields, qqGroupMessage{}); !reflect.DeepEqual(got, want) {
		t.Fatal("长引用的截断或附件结果改变")
	}
	// JSON 解码仍随输入增长，但不应再为最终会丢弃的全文建立拼接副本。
	allocated := (after.TotalAlloc - before.TotalAlloc) / runs
	if budget := uint64(len(fields.MsgElements)) * 3 / 2; allocated > budget {
		t.Fatalf("引用文字截断前不应拼接完整正文：分配=%d，预算=%d", allocated, budget)
	}
}

func BenchmarkQQQuotePrefix(b *testing.B) {
	many := make([]string, 8)
	for i := range many {
		many[i] = strings.Repeat("引用文字", 500)
	}
	for _, tc := range []struct {
		name     string
		contents []string
	}{
		{"单段短引用", []string{"候选后面的字母是什么意思"}},
		{"多段短引用", []string{"候选后面的字母", "按哪个键输入", "Windows 上的水杉输入法"}},
		{"单段长引用", []string{strings.Repeat("引用文字", 1000)}},
		{"多段长引用", many},
	} {
		fields := qqQuotePrefixFields(b, tc.contents)
		b.Run(tc.name, func(b *testing.B) {
			for _, impl := range []struct {
				name string
				run  func(qqQuoteEventFields, qqGroupMessage) qqQuote
			}{{"previous", previousQQQuoteFromFields}, {"current", qqQuoteFromFields}} {
				b.Run(impl.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = impl.run(fields, qqGroupMessage{})
					}
				})
			}
		})
	}
}
