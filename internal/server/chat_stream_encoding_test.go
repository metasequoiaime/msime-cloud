package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamOutputEncoding(t *testing.T) {
	for _, tc := range []struct {
		name, text string
	}{
		{"短事件", "你好"},
		{"JSON转义", "<>&\u2028\u2029\n\r\t\"\\字"},
		{"控制字符", "\x00\x01\b\t\n\f\r\x1f正文"},
		{"接近缓冲上限", strings.Repeat("字", 1300)},
		{"大空白前缀", strings.Repeat(" ", 8192)},
		{"转义后大事件", strings.Repeat("<&", 2048)},
	} {
		for _, oneByte := range []bool{false, true} {
			name := tc.name + "/整段读取"
			if oneByte {
				name = tc.name + "/逐字节读取"
			}
			t.Run(name, func(t *testing.T) {
				body := contentFrame(" ") + contentFrame(tc.text) + contentFrame("尾") + "data: [DONE]\n\n"
				want := contentPrefix(t, " ") + contentPrefix(t, tc.text) + contentPrefix(t, "尾") + "data: [DONE]\n\n"
				s := chatStreamContentServer(body)
				if oneByte {
					s.client.Transport = chatStreamDataByteTransport(body)
				}
				for range 2 {
					w := httptest.NewRecorder()
					s.chatStreamOnce(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), chatRequest{Model: "test-model", Stream: true}, false)
					if w.Code != 200 || w.Body.String() != want {
						t.Fatalf("流式编码结果变化：状态=%d，输出长度=%d，期望长度=%d", w.Code, w.Body.Len(), len(want))
					}
				}
			})
		}
	}
}
