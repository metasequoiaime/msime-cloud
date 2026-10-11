package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// chatReply 是一条合格的非流式回答。
func chatReply(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}}}})
	return string(b)
}

// modelUpstream 按请求里的 model 选出上游的回答方式，并按顺序记下每次请求的模型。
type modelUpstream struct {
	mu      sync.Mutex
	models  []string
	answers map[string]func(http.ResponseWriter)
}

func (u *modelUpstream) handle(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var v chatRequest
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Error(err)
			return
		}
		if len(v.Messages) != 1 || v.Messages[0].Content != "hello" {
			t.Errorf("messages changed: %+v", v.Messages)
		}
		u.mu.Lock()
		u.models = append(u.models, v.Model)
		answer := u.answers[v.Model]
		u.mu.Unlock()
		if answer == nil {
			t.Errorf("unexpected model %q", v.Model)
			w.WriteHeader(500)
			return
		}
		answer(w)
	}
}

func (u *modelUpstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

func answerJSON(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = io.WriteString(w, body) }
}

func answerStatus(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "synthetic-upstream-detail")
	}
}

// 非流式请求：主模型上游失败或回答不合格时换备用模型重试一次；备用模型也失败、没有配置备用模型或备用模型就是刚失败的模型时照常返回 502。
func TestChatFallbackModel(t *testing.T) {
	ok := answerJSON(chatReply("主模型的回答"))
	rescued := answerJSON(chatReply("备用模型的回答"))
	for name, tc := range map[string]struct {
		body     string
		fallback string
		models   []string
		answers  map[string]func(http.ResponseWriter)
		code     int
		reply    string
		calls    []string
		errors   int64
	}{
		"primary succeeds": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": ok},
			code:    200, reply: "主模型的回答", calls: []string{"server-model"},
		},
		"primary status fails": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(500), "fallback-model": rescued},
			code:    200, reply: "备用模型的回答", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"primary rejects the request": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(400), "fallback-model": rescued},
			code:    200, reply: "备用模型的回答", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"primary answers blank": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": answerJSON(chatReply("  \n")), "fallback-model": rescued},
			code:    200, reply: "备用模型的回答", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"primary answers invalid json": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": answerJSON("{synthetic-upstream-detail"), "fallback-model": rescued},
			code:    200, reply: "备用模型的回答", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"explicit default model": {
			body: `{"model":"server-model","messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model", models: []string{"other-model"},
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(503), "fallback-model": rescued},
			code:    200, reply: "备用模型的回答", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"explicit allowed model": {
			body: `{"model":"other-model","messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model", models: []string{"other-model"},
			answers: map[string]func(http.ResponseWriter){"other-model": answerStatus(503), "fallback-model": rescued},
			code:    200, reply: "备用模型的回答", calls: []string{"other-model", "fallback-model"}, errors: 1,
		},
		"both fail": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(500), "fallback-model": answerJSON(chatReply(""))},
			code:    502, calls: []string{"server-model", "fallback-model"}, errors: 2,
		},
		"no fallback configured": {
			body:    `{"messages":[{"role":"user","content":"hello"}]}`,
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(500)},
			code:    502, calls: []string{"server-model"}, errors: 1,
		},
		"fallback is the failed model": {
			body: `{"messages":[{"role":"user","content":"hello"}]}`, fallback: "server-model",
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(500)},
			code:    502, calls: []string{"server-model"}, errors: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			u := &modelUpstream{answers: tc.answers}
			s := fixture(t, u.handle(t))
			s.config.Admin.Enabled = true
			s.config.Chat.FallbackModel = tc.fallback
			s.config.Chat.Models = tc.models
			w := call(s, "POST", "/v1/chat/completions", tc.body)
			if w.Code != tc.code || strings.Contains(w.Body.String(), "synthetic-upstream-detail") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			if tc.code == 200 && w.Body.String() != chatReply(tc.reply) {
				t.Fatalf("reply %s", w.Body.String())
			}
			if tc.code == 502 && !strings.Contains(w.Body.String(), "upstream_failure") {
				t.Fatal(w.Body.String())
			}
			if got := u.calls(); strings.Join(got, ",") != strings.Join(tc.calls, ",") {
				t.Fatalf("upstream models %v, want %v", got, tc.calls)
			}
			if m := chatMetrics(s); m.Calls != int64(len(tc.calls)) || m.Errors != tc.errors {
				t.Fatalf("metrics %+v", m)
			}
		})
	}
}

// 主模型超时（504）时请求的截止时间已经用完，不再换备用模型。
func TestChatFallbackSkipsTimeouts(t *testing.T) {
	for name, body := range map[string]string{
		"non-streaming": `{"messages":[{"role":"user","content":"hello"}]}`,
		"streaming":     `{"messages":[{"role":"user","content":"hello"}],"stream":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			u := &modelUpstream{answers: map[string]func(http.ResponseWriter){"server-model": func(http.ResponseWriter) { time.Sleep(1500 * time.Millisecond) }}}
			s := fixture(t, u.handle(t))
			s.config.Chat.FallbackModel = "fallback-model"
			s.config.TimeoutSeconds = 1
			w := call(s, "POST", "/v1/chat/completions", body)
			if w.Code != 504 || !strings.Contains(w.Body.String(), "upstream_timeout") {
				t.Fatal(w.Code, w.Body.String())
			}
			if got := u.calls(); len(got) != 1 {
				t.Fatalf("upstream models %v", got)
			}
		})
	}
}

// 流式请求：第一个字节之前失败时换备用模型从头请求，客户端只看到备用模型的流；开始输出后失败不换模型。
func TestChatStreamFallbackModel(t *testing.T) {
	rescued := func(w http.ResponseWriter) {
		sse(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n", contentFrame("备用"), "data: [DONE]\n\n")
	}
	for name, tc := range map[string]struct {
		fallback string
		answers  map[string]func(http.ResponseWriter)
		code     int
		stream   string
		calls    []string
		errors   int64
	}{
		"primary status fails": {
			fallback: "fallback-model",
			answers:  map[string]func(http.ResponseWriter){"server-model": answerStatus(500), "fallback-model": rescued},
			code:     200, stream: "备用", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"primary errors before content": {
			fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": func(w http.ResponseWriter) {
				sse(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n", `data: {"error":{"message":"synthetic-upstream-detail"}}`+"\n\n")
			}, "fallback-model": rescued},
			code: 200, stream: "备用", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"primary answers blank": {
			fallback: "fallback-model",
			answers:  map[string]func(http.ResponseWriter){"server-model": func(w http.ResponseWriter) { sse(w, contentFrame("  "), "data: [DONE]\n\n") }, "fallback-model": rescued},
			code:     200, stream: "备用", calls: []string{"server-model", "fallback-model"}, errors: 1,
		},
		"primary fails after the first byte": {
			fallback: "fallback-model",
			answers: map[string]func(http.ResponseWriter){"server-model": func(w http.ResponseWriter) {
				sse(w, contentFrame("部分"), `data: {"error":{"message":"synthetic-upstream-detail"}}`+"\n\n")
			}},
			code: 200, stream: "部分", calls: []string{"server-model"}, errors: 1,
		},
		"both fail": {
			fallback: "fallback-model",
			answers:  map[string]func(http.ResponseWriter){"server-model": answerStatus(500), "fallback-model": answerStatus(502)},
			code:     502, calls: []string{"server-model", "fallback-model"}, errors: 2,
		},
		"no fallback configured": {
			answers: map[string]func(http.ResponseWriter){"server-model": answerStatus(500)},
			code:    502, calls: []string{"server-model"}, errors: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			u := &modelUpstream{answers: tc.answers}
			s := fixture(t, u.handle(t))
			s.config.Admin.Enabled = true
			s.config.Chat.FallbackModel = tc.fallback
			live := httptest.NewServer(s)
			defer live.Close()
			resp := postStream(t, context.Background(), live, streamBody)
			body, _ := io.ReadAll(resp.Body)
			// Close 会等到处理函数返回，此时记账已经结束。
			live.Close()
			if resp.StatusCode != tc.code || strings.Contains(string(body), "synthetic-upstream-detail") {
				t.Fatal(resp.StatusCode, string(body))
			}
			switch {
			case tc.code == 502:
				if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || !strings.Contains(string(body), "upstream_failure") {
					t.Fatal(resp.Header, string(body))
				}
			case len(tc.calls) == 1:
				if string(body) != contentPrefix(t, tc.stream)+chatStreamError {
					t.Fatalf("body %q", body)
				}
			default:
				role := `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
				if string(body) != role+contentPrefix(t, tc.stream)+"data: [DONE]\n\n" {
					t.Fatalf("body %q", body)
				}
			}
			if got := u.calls(); strings.Join(got, ",") != strings.Join(tc.calls, ",") {
				t.Fatalf("upstream models %v, want %v", got, tc.calls)
			}
			if m := chatMetrics(s); m.Calls != int64(len(tc.calls)) || m.Errors != tc.errors {
				t.Fatalf("metrics %+v", m)
			}
		})
	}
}

// chat.fallback_model 能从配置文件读入；含空白或超过 200 字节的模型名让服务拒绝启动。
func TestChatFallbackModelConfiguration(t *testing.T) {
	t.Setenv("CONFIG_TEST_TOKEN", strings.Repeat("a", 32))
	for fallback, valid := range map[string]bool{"fallback-model": true, "": true, "fallback model": false, "fallback-model\n": false, strings.Repeat("b", 201): false} {
		path := filepath.Join(t.TempDir(), "config.json")
		chat, _ := json.Marshal(map[string]string{"url": "https://example.com/v1/chat/completions", "model": "primary-model", "fallback_model": fallback})
		config := `{"clients":[{"id":"local","token_env":"CONFIG_TEST_TOKEN","requests_per_minute":1}],"chat":` + string(chat) + `}`
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(path)
		if valid && (err != nil || c.Chat.Model != "primary-model" || c.Chat.FallbackModel != fallback) {
			t.Fatal(fallback, c.Chat, err)
		}
		if !valid && err == nil {
			t.Fatalf("fallback_model %q accepted", fallback)
		}
	}
}
