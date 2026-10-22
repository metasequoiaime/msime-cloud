package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func chatStreamDataFrames(multiline bool) string {
	if multiline {
		return "data: {\"choices\":\n: heartbeat\ndata: [{\"delta\":\ndata: {\"content\":\"字\"}}]}\n\n"
	}
	return "data: {\"choices\":[{\"delta\":{\"content\":\"字\"}}]}\n\n"
}

func BenchmarkChatStreamDataBuffer(b *testing.B) {
	for _, multiline := range []bool{false, true} {
		name := "单行事件"
		if multiline {
			name = "多行事件"
		}
		b.Run(name, func(b *testing.B) {
			s := chatStreamContentServer(strings.Repeat(chatStreamDataFrames(multiline), 512) + "data: [DONE]\n\n")
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				s.chatStreamOnce(w, r, chatRequest{Model: "test-model", Stream: true}, false)
				if w.Code != http.StatusOK || !bytes.HasSuffix(w.Body.Bytes(), []byte("data: [DONE]\n\n")) {
					b.Fatal("流式响应未完成")
				}
			}
		})
	}
}

func TestChatStreamDataBufferAllocations(t *testing.T) {
	for _, multiline := range []bool{false, true} {
		name := "单行事件"
		if multiline {
			name = "多行事件"
		}
		t.Run(name, func(t *testing.T) {
			s := chatStreamContentServer(strings.Repeat(chatStreamDataFrames(multiline), 512) + "data: [DONE]\n\n")
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			valid := true
			allocs := testing.AllocsPerRun(20, func() {
				w := httptest.NewRecorder()
				s.chatStreamOnce(w, r, chatRequest{Model: "test-model", Stream: true}, false)
				valid = valid && w.Code == http.StatusOK && bytes.HasSuffix(w.Body.Bytes(), []byte("data: [DONE]\n\n"))
			})
			if !valid {
				t.Fatal("流式响应未完成")
			}
			budget := float64(6400)
			if raceEnabled {
				// 竞态构建的编码缓冲池会随机丢弃对象，额外分配需要单独预留余量。
				budget = 7600
			}
			if allocs > budget {
				t.Fatalf("512 个事件分配 %.0f 次，超过 %.0f 次预算", allocs, budget)
			}
		})
	}
}

type chatStreamDataByteTransport string

func (body chatStreamDataByteTransport) RoundTrip(*http.Request) (*http.Response, error) {
	reader := iotest.OneByteReader(strings.NewReader(string(body)))
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(reader)}, nil
}

func TestChatStreamDataBufferBoundaries(t *testing.T) {
	first := contentFrame("首段")
	multi := strings.Replace(strings.TrimSuffix(first, "\n\n"), `"choices":[`, "\"choices\":\ndata: [", 1) + "\n: heartbeat\ndata:\n\n"
	multi = "data:\n" + multi
	longText := strings.Repeat("大", 8192)
	finish := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}"
	finishOut := "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	for _, tc := range []struct {
		name, body, want string
		code             int
	}{
		{"多行JSON与空data行", multi + contentFrame("尾段") + "data: [DONE]\n\n", contentPrefix(t, "首段") + contentPrefix(t, "尾段") + "data: [DONE]\n\n", 200},
		{"混合换行", strings.ReplaceAll(multi, "\n", "\r\n") + strings.ReplaceAll(contentFrame("尾段"), "\n", "\r") + "data: [DONE]\r\r", contentPrefix(t, "首段") + contentPrefix(t, "尾段") + "data: [DONE]\n\n", 200},
		{"大事件后复用缓冲", contentFrame(longText) + strings.Repeat(contentFrame("短"), 8) + "data: [DONE]\n\n", contentPrefix(t, longText) + strings.Repeat(contentPrefix(t, "短"), 8) + "data: [DONE]\n\n", 200},
		{"空data事件仍失败", "data: \n\n" + first + "data: [DONE]\n\n", "", 502},
		{"无data的事件可忽略", "event: ignored\nid: 1\nretry: 10\n\n" + first + "data: [DONE]\n\n", contentPrefix(t, "首段") + "data: [DONE]\n\n", 200},
		{"流开始后空data仍失败", first + "data:\n\n", contentPrefix(t, "首段") + chatStreamError, 200},
		{"EOF分发DONE", first + "data: [DONE]", contentPrefix(t, "首段") + "data: [DONE]\n\n", 200},
		{"EOF分发结束事件", first + finish, contentPrefix(t, "首段") + finishOut + "data: [DONE]\n\n", 200},
	} {
		for _, oneByte := range []bool{false, true} {
			name := tc.name + "/整段读取"
			if oneByte {
				name = tc.name + "/逐字节读取"
			}
			t.Run(name, func(t *testing.T) {
				s := chatStreamContentServer(tc.body)
				if oneByte {
					s.client.Transport = chatStreamDataByteTransport(tc.body)
				}
				w := httptest.NewRecorder()
				s.chatStreamOnce(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), chatRequest{Model: "test-model", Stream: true}, false)
				if w.Code != tc.code {
					t.Fatalf("响应状态 %d，期望 %d", w.Code, tc.code)
				}
				if tc.code == 502 {
					if !strings.Contains(w.Body.String(), "upstream_failure") || strings.Contains(w.Body.String(), "chat.completion.chunk") {
						t.Fatal("空data事件没有返回通用错误")
					}
				} else if w.Body.String() != tc.want {
					t.Fatalf("SSE输出变化：实际 %d B，期望 %d B", w.Body.Len(), len(tc.want))
				}
			})
		}
	}
}
