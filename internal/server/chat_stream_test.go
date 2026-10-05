package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

const streamBody = `{"messages":[{"role":"user","content":"hello"}],"stream":true}`

// postStream 通过真实的 HTTP 服务发起流式请求，返回尚未读取的响应，让测试逐个事件读取。
func postStream(t *testing.T, ctx context.Context, live *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST", live.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := live.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readEvent 读出下一个 SSE 事件的 data。
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read event: %v (%q)", err, line)
	}
	blank, err := r.ReadString('\n')
	if err != nil || blank != "\n" || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("malformed event %q %q %v", line, blank, err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
}

func eventContent(t *testing.T, data string) string {
	t.Helper()
	var chunk struct {
		Object  string `json:"object"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
		t.Fatalf("not a chat.completion.chunk: %s", data)
	}
	return chunk.Choices[0].Delta.Content
}

func chatMetrics(s *Server) account.ServiceMetric {
	for _, row := range s.metrics.pending() {
		if row.Service == "chat" {
			return row
		}
	}
	return account.ServiceMetric{}
}

func sse(w http.ResponseWriter, frames ...string) {
	for _, frame := range frames {
		_, _ = io.WriteString(w, frame)
	}
	w.(http.Flusher).Flush()
}

func contentFrame(text string) string {
	b, _ := json.Marshal(map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "server-model", "provider_private": "synthetic-private", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}}})
	return "data: " + string(b) + "\n\n"
}

// 上游逐个事件到达时，客户端也逐个收到；结束时有 `[DONE]`，并计一次成功调用。
func TestChatStreamRelaysEventsIncrementally(t *testing.T) {
	next := make(chan struct{})
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["stream"] != true || body["model"] != "server-model" {
			t.Errorf("upstream request %v %v", body, err)
		}
		if _, ok := body["stream_options"]; ok {
			t.Error("stream_options forwarded")
		}
		if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("wrong upstream headers")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, ": keep-alive\n\n", `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n", contentFrame("你"))
		<-next
		sse(w, "event: message\r\n"+strings.TrimSuffix(contentFrame("好"), "\n\n")+"\r\n\r\n", `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
		<-next
		sse(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n", "data: [DONE]\n\n")
	})
	s.config.Admin.Enabled = true
	live := httptest.NewServer(s)
	defer live.Close()
	resp := postStream(t, context.Background(), live, streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatal(resp.StatusCode, resp.Header)
	}
	r := bufio.NewReader(resp.Body)
	// 角色事件缓存到第一段非空正文才一起发出。
	if got := eventContent(t, readEvent(t, r)); got != "" {
		t.Fatalf("role event carried %q", got)
	}
	first := readEvent(t, r)
	if eventContent(t, first) != "你" || strings.Contains(first, "synthetic-private") {
		t.Fatalf("first content event %s", first)
	}
	next <- struct{}{}
	if got := eventContent(t, readEvent(t, r)); got != "好" {
		t.Fatalf("second content event %q", got)
	}
	next <- struct{}{}
	finish := readEvent(t, r)
	if !strings.Contains(finish, `"finish_reason":"stop"`) {
		t.Fatalf("finish event %s", finish)
	}
	if done := readEvent(t, r); done != "[DONE]" {
		t.Fatalf("last event %q", done)
	}
	if rest, _ := io.ReadAll(r); len(rest) != 0 {
		t.Fatalf("trailing bytes %q", rest)
	}
	if m := chatMetrics(s); m.Calls != 1 || m.Errors != 0 {
		t.Fatalf("metrics %+v", m)
	}
}

// 流开始后上游失败：已经发出的内容保留，最后一个事件是通用错误，没有 `[DONE]`，也不透传上游正文。
func TestChatStreamUpstreamErrorMidStream(t *testing.T) {
	for name, tail := range map[string]string{
		"error event":   `data: {"error":{"message":"synthetic-upstream-detail","type":"server_error"}}` + "\n\n",
		"invalid event": "data: {synthetic-upstream-detail\n\n",
		"truncated":     "",
	} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				sse(w, contentFrame("部分"), tail)
			})
			s.config.Admin.Enabled = true
			live := httptest.NewServer(s)
			defer live.Close()
			resp := postStream(t, context.Background(), live, streamBody)
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			want := contentPrefix(t, "部分") + chatStreamError
			if string(body) != want {
				t.Fatalf("body %q, want %q", body, want)
			}
			if m := chatMetrics(s); m.Calls != 1 || m.Errors != 1 {
				t.Fatalf("metrics %+v", m)
			}
		})
	}
}

// contentPrefix 是本服务为一段正文重新编码出的事件。
func contentPrefix(t *testing.T, text string) string {
	t.Helper()
	b, _ := json.Marshal(chatStreamOut{ID: "chatcmpl-1", Object: "chat.completion.chunk", Model: "server-model", Choices: []chatStreamChoice{{Delta: chatStreamDelta{Content: &text}}}})
	return "data: " + string(b) + "\n\n"
}

// 第一个字节之前的失败沿用非流式的 JSON 错误响应。
func TestChatStreamFailuresBeforeFirstByte(t *testing.T) {
	for name, tc := range map[string]struct {
		upstream func(http.ResponseWriter)
		code     int
		error    string
	}{
		"status": {func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = io.WriteString(w, "synthetic-upstream-detail") }, 502, "upstream_failure"},
		"json body": {func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"synthetic-upstream-detail"}}]}`)
		}, 502, "upstream_failure"},
		"empty answer":  {func(w http.ResponseWriter) { sse(w, contentFrame("  "), "data: [DONE]\n\n") }, 502, "upstream_failure"},
		"early error":   {func(w http.ResponseWriter) { sse(w, `data: {"error":{"message":"synthetic-upstream-detail"}}`+"\n\n") }, 502, "upstream_failure"},
		"oversize line": {func(w http.ResponseWriter) { sse(w, "data: "+strings.Repeat("x", contract.UpstreamResponseBytes+1)) }, 502, "upstream_failure"},
	} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) { tc.upstream(w) })
			s.config.Admin.Enabled = true
			w := call(s, "POST", "/v1/chat/completions", streamBody)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.error) || strings.Contains(w.Body.String(), "synthetic-upstream-detail") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			if m := chatMetrics(s); m.Calls != 1 || m.Errors != 1 {
				t.Fatalf("metrics %+v", m)
			}
		})
	}
}

// 正文超过 `OutputTextBytes` 时结束流；请求截止时间同样约束整条流。
func TestChatStreamLimits(t *testing.T) {
	t.Run("output text", func(t *testing.T) {
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			sse(w, contentFrame("开始"))
			for range contract.OutputTextBytes / 4096 {
				sse(w, contentFrame(strings.Repeat("x", 4096)))
			}
			sse(w, "data: [DONE]\n\n")
		})
		live := httptest.NewServer(s)
		defer live.Close()
		body, _ := io.ReadAll(postStream(t, context.Background(), live, streamBody).Body)
		if !strings.HasSuffix(string(body), chatStreamError) || strings.Contains(string(body), "[DONE]") {
			t.Fatalf("stream not cut: %q", body[len(body)-200:])
		}
	})
	t.Run("upstream bytes", func(t *testing.T) {
		// 注释行不产生内容，但同样计入整条流的字节上限。
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			sse(w, contentFrame("开始"))
			heartbeat := ": " + strings.Repeat("x", 1022) + "\n"
			for range contract.UpstreamResponseBytes/len(heartbeat) + 1 {
				sse(w, heartbeat)
			}
			sse(w, "data: [DONE]\n\n")
		})
		live := httptest.NewServer(s)
		defer live.Close()
		body, _ := io.ReadAll(postStream(t, context.Background(), live, streamBody).Body)
		if string(body) != contentPrefix(t, "开始")+chatStreamError {
			t.Fatalf("stream not cut: %q", body)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			sse(w, contentFrame("开始"))
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		s.config.TimeoutSeconds = 1
		s.config.Admin.Enabled = true
		live := httptest.NewServer(s)
		defer live.Close()
		started := time.Now()
		body, _ := io.ReadAll(postStream(t, context.Background(), live, streamBody).Body)
		if string(body) != contentPrefix(t, "开始")+chatStreamError || time.Since(started) > 10*time.Second {
			t.Fatalf("%q after %s", body, time.Since(started))
		}
		if m := chatMetrics(s); m.Calls != 1 || m.Errors != 1 {
			t.Fatalf("metrics %+v", m)
		}
	})
}

// 客户端断开时取消上游请求，放弃的流不计入调用。
func TestChatStreamClientCancelStopsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, contentFrame("开始"))
		select {
		case <-r.Context().Done():
			close(upstreamDone)
		case <-time.After(10 * time.Second):
			t.Error("upstream request was not cancelled")
		}
	})
	s.config.Admin.Enabled = true
	live := httptest.NewServer(s)
	defer live.Close()
	ctx, cancel := context.WithCancel(context.Background())
	resp := postStream(t, ctx, live, streamBody)
	if got := eventContent(t, readEvent(t, bufio.NewReader(resp.Body))); got != "开始" {
		t.Fatal(got)
	}
	cancel()
	select {
	case <-upstreamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream still running after the client left")
	}
	// Close 会等到处理函数返回，此时记账已经结束。
	live.Close()
	if m := chatMetrics(s); m.Calls != 0 {
		t.Fatalf("abandoned stream was metered: %+v", m)
	}
}

// 不带 stream 或 stream 为 false 的请求仍是原来的 JSON 透传，上游请求里也不出现 stream 字段。
func TestChatWithoutStreamIsUnchanged(t *testing.T) {
	for _, body := range []string{`{"messages":[{"role":"user","content":"hello"}]}`, `{"messages":[{"role":"user","content":"hello"}],"stream":false}`} {
		s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), "stream") || r.Header.Get("Accept") != "application/json" {
				t.Errorf("upstream request %s", raw)
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"你好"}}]}`)
		})
		w := call(s, "POST", "/v1/chat/completions", body)
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Body.String() != `{"choices":[{"message":{"role":"assistant","content":"你好"}}]}` {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
	}
}

// 流式请求与非流式一样先过校验：无效请求、未允许的模型都在上游之前被拒绝。
func TestChatStreamValidatesLikeNonStreaming(t *testing.T) {
	s := fixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached upstream") })
	s.config.Chat.Models = []string{"allowed-model"}
	for body, code := range map[string]string{
		`{"messages":[],"stream":true}`: "invalid_chat_request",
		`{"model":"other","messages":[{"role":"user","content":"hello"}],"stream":true}`:          "unsupported_model",
		`{"messages":[{"role":"user","content":"hello"}],"stream":true,"temperature":3}`:          "invalid_temperature",
		`{"messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{}}`:      "invalid_json",
		`{"messages":[{"role":"user","content":"hello"}],"stream":true,"max_tokens":999999}`:      "invalid_chat_request",
		`{"messages":[{"role":"tool","content":"hello"}],"stream":true}`:                          "invalid_message",
		`{"messages":[{"role":"user","content":"hello"}],"stream":true,"thinking":{"type":"on"}}`: "unsupported_thinking_mode",
	} {
		if w := call(s, "POST", "/v1/chat/completions", body); w.Code != 400 || !strings.Contains(w.Body.String(), code) {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
}
