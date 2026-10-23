package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

type chatStreamContentTransport string

func (body chatStreamContentTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func chatStreamContentServer(body string) *Server {
	return &Server{
		config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://chat.example.test/completions"}}},
		client: &http.Client{Transport: chatStreamContentTransport(body)},
	}
}

func chatStreamContentFrames(text string, count int, tail string) string {
	raw, _ := json.Marshal(text)
	frame := "data: {\"choices\":[{\"delta\":{\"content\":" + string(raw) + "}}]}\n\n"
	return strings.Repeat(frame, count) + tail + "data: [DONE]\n\n"
}

func BenchmarkChatStreamContent(b *testing.B) {
	for _, tc := range []struct {
		name, text string
		count      int
		tail       string
	}{
		{"短正文", "你好", 8, ""},
		{"长空白前缀", strings.Repeat("\u2003", 32), 512, contentFrame("答")},
		{"正文接近长度上限", strings.Repeat("字", 1024) + "x", 21, contentFrame("x")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			body := chatStreamContentFrames(tc.text, tc.count, tc.tail)
			if len(body) > contract.UpstreamResponseBytes {
				b.Fatal("测试流超过上游字节上限")
			}
			s := chatStreamContentServer(body)
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				s.chatStreamOnce(w, r, chatRequest{Model: "test-model", Stream: true}, false)
				if w.Code != http.StatusOK || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					b.Fatal("流式响应未正常完成")
				}
			}
		})
	}
}

func TestChatStreamContentMemory(t *testing.T) {
	body := chatStreamContentFrames(strings.Repeat("字", 1024)+"x", 21, contentFrame("x"))
	s := chatStreamContentServer(body)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			w := httptest.NewRecorder()
			s.chatStreamOnce(w, r, chatRequest{Model: "test-model", Stream: true}, false)
			if w.Code != http.StatusOK || !bytes.HasSuffix(w.Body.Bytes(), []byte("data: [DONE]\n\n")) {
				b.Fatal("流式响应未正常完成")
			}
		}
	})
	budget := int64(700 * 1024)
	if raceEnabled {
		// 竞态构建的 sync.Pool 会随机丢弃编码缓冲；留出余量，但仍拒绝旧实现的正文缓存开销。
		budget = 960 * 1024
	}
	if allocated := result.AllocedBytesPerOp(); allocated > budget {
		t.Fatalf("约 64 KiB 正文分配 %d B，超过 %d KiB 预算", allocated, budget/1024)
	}
}

func TestChatStreamContentBoundaries(t *testing.T) {
	spaces := " \t\r\n\v\f\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	for _, tc := range []struct {
		name      string
		chunks    []string
		code      int
		wantError bool
		noDone    bool
	}{
		{"空白前缀与后缀", []string{"", spaces, "\u2003", "正文", spaces, ""}, 200, false, false},
		{"全部为空白", []string{"", spaces, "\u2003"}, 502, true, false},
		{"零宽空格不是空白", []string{spaces, "\u200b", spaces}, 200, false, false},
		{"正文BOM不是空白", []string{spaces, "\ufeff"}, 200, false, false},
		{"正文恰好达到字节上限", []string{strings.Repeat(" ", contract.OutputTextBytes-3), "答"}, 200, false, false},
		{"首段正文超出一个字节", []string{strings.Repeat(" ", contract.OutputTextBytes-2), "答"}, 502, true, false},
		{"流开始后超出一个字节", []string{"答", strings.Repeat(" ", contract.OutputTextBytes-2)}, 200, true, false},
		{"结束原因代替DONE", []string{spaces, "正文", spaces}, 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upstream, want strings.Builder
			for _, chunk := range tc.chunks {
				upstream.WriteString(contentFrame(chunk))
				want.WriteString(contentPrefix(t, chunk))
			}
			if tc.noDone {
				upstream.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				want.WriteString("data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			} else {
				upstream.WriteString("data: [DONE]\n\n")
			}
			want.WriteString("data: [DONE]\n\n")
			s := chatStreamContentServer(upstream.String())
			w := httptest.NewRecorder()
			s.chatStreamOnce(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), chatRequest{Model: "test-model", Stream: true}, false)
			if w.Code != tc.code {
				t.Fatalf("响应状态 %d，期望 %d", w.Code, tc.code)
			}
			got := w.Body.String()
			if tc.wantError {
				if tc.code == 502 {
					if !strings.Contains(got, "upstream_failure") || strings.Contains(got, "chat.completion.chunk") {
						t.Fatal("首字节前的失败没有返回通用 JSON 错误")
					}
				} else if got != contentPrefix(t, tc.chunks[0])+chatStreamError {
					t.Fatal("流开始后的长度超限响应变化")
				}
			} else if got != want.String() {
				t.Fatalf("事件输出变化：实际 %d B，期望 %d B", len(got), want.Len())
			}
		})
	}
}
