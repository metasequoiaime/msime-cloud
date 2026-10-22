package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamOutputReuseFields(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"first","created":123,"model":"first-model","choices":[{"index":9,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[],"usage":{"completion_tokens":1}}`,
		`data: {"choices":[{"index":7,"delta":{"content":"正文"}},{"index":8,"delta":{"content":"不应转发"}}]}`,
		`data: {"id":"second","model":"second-model","choices":[{"delta":{"role":"assistant"},"finish_reason":"stop"}]}`,
		`data: {"choices":[{"delta":{"content":""},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":null}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	want := strings.Join([]string{
		`data: {"id":"first","object":"chat.completion.chunk","created":123,"model":"first-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"正文"},"finish_reason":null}]}`,
		`data: {"id":"second","object":"chat.completion.chunk","model":"second-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}]}`,
		`data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	s := chatStreamContentServer(input)
	for _, oneByte := range []bool{false, true} {
		name := "整段读取"
		if oneByte {
			name = "逐字节读取"
		}
		t.Run(name, func(t *testing.T) {
			if oneByte {
				s.client.Transport = chatStreamDataByteTransport(input)
			}
			for range 2 {
				w := httptest.NewRecorder()
				s.chatStreamOnce(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), chatRequest{Model: "test-model", Stream: true}, false)
				if w.Code != 200 || w.Body.String() != want {
					t.Fatalf("复用输出容器后事件字段变化：状态=%d，输出=%s", w.Code, w.Body.String())
				}
			}
		})
	}
}
