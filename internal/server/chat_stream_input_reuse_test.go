package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreamInputReuseFields(t *testing.T) {
	first := `data: {"id":"first","created":123,"model":"first-model","choices":[{"index":9,"delta":{"role":"assistant","content":"首"},"finish_reason":"length"}]}` + "\n\n"
	firstOut := `data: {"id":"first","object":"chat.completion.chunk","created":123,"model":"first-model","choices":[{"index":0,"delta":{"role":"assistant","content":"首"},"finish_reason":"length"}]}` + "\n\n"
	tailOut := `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"尾"},"finish_reason":null}]}` + "\n\n"
	lastOut := `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"末"},"finish_reason":null}]}` + "\n\n"
	for _, tc := range []struct {
		name, event, output string
	}{
		{"缺失元数据", `{"choices":[{"delta":{"content":"尾"}}]}`, tailOut},
		{"空值元数据", `{"id":null,"created":null,"model":null,"choices":[{"delta":{"content":"尾"}}]}`, tailOut},
		{"缺失选项", `{"usage":{"completion_tokens":1}}`, ""},
		{"空值事件", `null`, ""},
		{"空值选项", `{"choices":null}`, ""},
		{"空选项数组", `{"choices":[]}`, ""},
		{"空值选项元素", `{"choices":[null]}`, ""},
		{"仅转发首个选项", `{"choices":[{"delta":{"content":"尾"}},{"delta":{"role":"assistant","content":"不应转发"},"finish_reason":"stop"}]}`, tailOut},
		{"首个选项为空值", `{"choices":[null,{"delta":{"content":"不应转发"}}]}`, ""},
		{"大量空选项", `{"choices":[{}` + strings.Repeat(`,{}`, 127) + `]}`, ""},
		{"空增量", `{"choices":[{"delta":{}}]}`, ""},
		{"空值增量", `{"choices":[{"delta":null}]}`, ""},
		{"空值增量字段", `{"choices":[{"delta":{"role":null,"content":null},"finish_reason":null}]}`, ""},
		{"仅角色", `{"choices":[{"delta":{"role":"user"}}]}`, `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"user"},"finish_reason":null}]}` + "\n\n"},
		{"仅结束原因", `{"choices":[{"finish_reason":"stop"}]}`, `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"},
		{"同事件重复增量", `{"choices":[{"delta":{"role":"assistant"},"delta":{"content":"尾"}}]}`, `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"尾"},"finish_reason":null}]}` + "\n\n"},
		{"同事件重复选项", `{"choices":[{"delta":{"role":"assistant"}}],"choices":[{"delta":{"content":"尾"}}]}`, `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"尾"},"finish_reason":null}]}` + "\n\n"},
		{"同事件选项缩短", `{"choices":[{"delta":{"role":"assistant"}},{"delta":{"content":"不应转发"}}],"choices":[{"delta":{"content":"尾"}}]}`, `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"尾"},"finish_reason":null}]}` + "\n\n"},
		{"同事件选项清空后恢复", `{"choices":[{"delta":{"role":"assistant"}}],"choices":[],"choices":[{"delta":{"content":"尾"}}]}`, tailOut},
		{"同事件选项空值后恢复", `{"choices":[{"delta":{"role":"assistant"}}],"choices":null,"choices":[{"delta":{"content":"尾"}}]}`, tailOut},
		{"空值错误", `{"error":null,"choices":[{"delta":{"content":"尾"}}]}`, tailOut},
	} {
		for _, oneByte := range []bool{false, true} {
			name := tc.name + "/整段读取"
			if oneByte {
				name = tc.name + "/逐字节读取"
			}
			t.Run(name, func(t *testing.T) {
				body := first + "data: " + tc.event + "\n\n" + `data: {"choices":[{"delta":{"content":"末"}}]}` + "\n\n" + "data: [DONE]\n\n"
				want := firstOut + tc.output + lastOut + "data: [DONE]\n\n"
				s := chatStreamContentServer(body)
				if oneByte {
					s.client.Transport = chatStreamDataByteTransport(body)
				}
				for range 2 {
					w := httptest.NewRecorder()
					s.chatStreamOnce(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), chatRequest{Model: "test-model", Stream: true}, false)
					if w.Code != 200 || w.Body.String() != want {
						t.Fatalf("复用解码容器后事件字段变化：状态=%d，输出=%s", w.Code, strings.TrimSpace(w.Body.String()))
					}
				}
			})
		}
	}
}
