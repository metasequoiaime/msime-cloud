package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func writeInvalidTrailerResponse(t *testing.T, w http.ResponseWriter, contentType, body string) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	// 正文分块合法，非法 trailer 使读取错误包含上游回显的合成用户输入。
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Synthetic\r\nContent-Type: %s\r\n\r\n%x\r\n%s\r\n0\r\nsynthetic-private-user-input\r\n\r\n", contentType, len(body), body); err != nil {
		t.Error(err)
	}
}

func TestJSONUpstreamBodyReadErrorsDoNotExposeContent(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		writeInvalidTrailerResponse(t, w, "application/json", `{}`)
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/cloud/candidates", nil)
	r.Pattern = "GET /v1/cloud/candidates"
	mr, meter := metered(r, "cloud", 0)
	req, err := http.NewRequestWithContext(mr.Context(), http.MethodGet, s.config.Cloud.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.doUpstream(req)
	var original textproto.ProtocolError
	if !errors.As(err, &original) || !strings.Contains(original.Error(), "synthetic-private-user-input") {
		t.Fatal("丢失了正文读取的原始协议错误", err)
	}
	if len(body) != 0 || !meter.sent || meter.err != err {
		t.Fatal("正文读取失败返回了不完整正文或没有记录计量错误", body, meter)
	}
	logs := captureLogs(t)
	w := httptest.NewRecorder()
	upstreamError(w, mr, err)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream_failure") || !strings.Contains(logs.String(), "upstream failed") {
		t.Fatal("正文读取失败没有正确归类", w.Code, w.Body.String(), logs.String())
	}
	if strings.Contains(err.Error(), "synthetic-private-user-input") || strings.Contains(logs.String(), "synthetic-private-user-input") || strings.Contains(w.Body.String(), "synthetic-private-user-input") {
		t.Fatal("共享上游正文读取错误泄露了上游内容", err, logs.String(), w.Body.String())
	}
}

func TestQQFallbackDoesNotLogUpstreamBodyReadError(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input chatRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input.Model == "server-model" {
			writeInvalidTrailerResponse(t, w, "application/json", `{}`)
			return
		}
		respond(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"answer":"合成备用回答"}`}, "finish_reason": "stop"}}})
	})
	s.qq = &qqBot{config: QQBotConfig{Model: "server-model", FallbackModel: "fallback-model"}}
	logs := captureLogs(t)
	var out struct{ Answer string }
	if err := s.qqChatModel(t.Context(), "server-model", "合成系统说明", "合成用户输入", &out); err != nil || out.Answer != "合成备用回答" {
		t.Fatal(out, err)
	}
	if !strings.Contains(logs.String(), "trying the fallback model") || strings.Contains(logs.String(), "synthetic-private-user-input") {
		t.Fatal("QQ 正文读取失败的回退日志未脱敏", logs.String())
	}
}

func TestAdminIssueBodyReadFailureDoesNotLogUpstreamContent(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		writeInvalidTrailerResponse(t, w, "application/json", `[]`)
	})
	client := &githubapp.Client{APIURL: s.config.Chat.URL, HTTP: s.client, Cache: &githubapp.Cache{}}
	logs := captureLogs(t)
	_, _, err := s.issuePage(t.Context(), client, "synthetic-token", "o/r", "/repos/o/r/issues")
	if err == nil {
		t.Fatal("正文读取失败没有导致 issue 查询失败")
	}
	w := httptest.NewRecorder()
	issueFail(w, err)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "github_unavailable") || !strings.Contains(logs.String(), "issues: GitHub unavailable") {
		t.Fatal("没有正确归类正文读取失败", w.Code, w.Body.String(), logs.String())
	}
	if strings.Contains(logs.String(), "synthetic-private-user-input") || strings.Contains(w.Body.String(), "synthetic-private-user-input") {
		t.Fatal("后台 issue 正文读取错误泄露了上游内容", logs.String(), w.Body.String())
	}
}

func TestChatStreamBodyReadFailureDoesNotLogUpstreamContent(t *testing.T) {
	for _, scenario := range []string{"before-streaming", "fallback", "after-streaming"} {
		t.Run(scenario, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				var input chatRequest
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					return
				}
				if input.Model == "fallback-model" {
					if scenario != "fallback" {
						t.Error("流开始后的失败不应触发备用模型")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, contentFrame("合成备用回答")+"data: [DONE]\n\n")
					return
				}
				body := ": heartbeat\n\n"
				if scenario == "after-streaming" {
					body = contentFrame("合成部分回答")
				}
				writeInvalidTrailerResponse(t, w, "text/event-stream", body)
			})
			if scenario != "before-streaming" {
				s.config.Chat.FallbackModel = "fallback-model"
			}
			logs := captureLogs(t)
			w := call(s, http.MethodPost, "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"合成输入"}]}`)
			switch scenario {
			case "before-streaming":
				if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream_failure") || !strings.Contains(logs.String(), "upstream failed") {
					t.Fatal("流开始前的正文读取失败没有正确归类", w.Code, w.Body.String(), logs.String())
				}
			case "fallback":
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "合成备用回答") || !strings.Contains(logs.String(), "trying the fallback model") {
					t.Fatal("流开始前的正文读取失败没有切换备用模型", w.Code, w.Body.String(), logs.String())
				}
			case "after-streaming":
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "合成部分回答") || !strings.Contains(w.Body.String(), chatStreamError) || strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(logs.String(), "upstream stream failed") {
					t.Fatal("流开始后的正文读取失败没有结束为错误事件", w.Code, w.Body.String(), logs.String())
				}
			}
			if strings.Contains(logs.String(), "synthetic-private-user-input") || strings.Contains(w.Body.String(), "synthetic-private-user-input") {
				t.Fatal("流式聊天正文读取错误泄露了上游内容", logs.String(), w.Body.String())
			}
		})
	}
}

func TestAdminLogsRedirectFailureDoesNotLogUpstreamURL(t *testing.T) {
	s, _ := adminLogsFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		location := "https://synthetic-user:synthetic-private-key@example.test/%invalid?query=" + url.QueryEscape(r.URL.Query().Get("query"))
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	s.config.Admin.Logs.LokiURL = upstream.URL
	s.loki = upstream.Client()
	s.loki.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	logs := captureLogs(t)
	w := adminLogsGet(s, "/api/logs?q=synthetic-private-input", ownerCookie, "")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "logs_unavailable") || !strings.Contains(logs.String(), "admin logs: loki query failed") || !strings.Contains(logs.String(), "loki unreachable") {
		t.Fatalf("没有正确归类重定向解析失败：status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
	}
	for _, private := range []string{"synthetic-private-input", "synthetic-private-key", "synthetic-user", "example.test", "%invalid"} {
		if strings.Contains(logs.String(), private) || strings.Contains(w.Body.String(), private) {
			t.Errorf("Loki 重定向错误泄露了 %q：logs=%s body=%s", private, logs.String(), w.Body.String())
		}
	}
}

func TestAdminIssueDecodeFailureDoesNotLogUpstreamBody(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"number":1,"created_at":"synthetic-private-input"}]`)
	})
	client := &githubapp.Client{APIURL: s.config.Chat.URL, HTTP: s.client, Cache: &githubapp.Cache{}}
	logs := captureLogs(t)
	_, _, err := s.issuePage(t.Context(), client, "synthetic-token", "o/r", "/repos/o/r/issues")
	if err == nil {
		t.Fatal("无效的 issue 正文没有导致查询失败")
	}
	w := httptest.NewRecorder()
	issueFail(w, err)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "github_unavailable") || !strings.Contains(logs.String(), "issues: GitHub unavailable") {
		t.Fatalf("没有正确归类解码失败：status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
	}
	if strings.Contains(logs.String(), "synthetic-private-input") || strings.Contains(w.Body.String(), "synthetic-private-input") {
		t.Fatal("后台 issue 解码错误泄露了上游字段值", logs.String(), w.Body.String())
	}
}

func TestQQIssueDecodeFailureDoesNotLogUpstreamBody(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/77/access_tokens" {
			respond(w, http.StatusCreated, map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		io.WriteString(w, `{"number":987654321098765432109876543210}`)
	})
	s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: s.config.Chat.URL, HTTP: s.client, Cache: &githubapp.Cache{}}
	s.qq = &qqBot{config: QQBotConfig{Repo: "o/r"}}
	logs := captureLogs(t)
	if result := s.qqIssueStatus(t.Context(), "", "1", 0); !strings.Contains(result, "暂时查不了") {
		t.Fatal("无效的 issue 正文没有导致查询失败", result)
	}
	if !strings.Contains(logs.String(), "issue lookup failed") {
		t.Fatal("没有记录 issue 解码失败", logs.String())
	}
	if strings.Contains(logs.String(), "987654321098765432109876543210") {
		t.Fatal("QQ issue 解码错误泄露了上游字段值", logs.String())
	}
}

func TestQQIssueSearchDoesNotLogUpstreamURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/77/access_tokens" {
			respond(w, http.StatusCreated, map[string]any{"token": "synthetic-token", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		if r.URL.Path != "/search/issues" {
			t.Error("出现意外的 GitHub 请求", r.URL.Path)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	s.adminGitHub = &githubapp.Client{AppID: 42, InstallationID: 77, Key: wordsKey(t), APIURL: s.config.Chat.URL, HTTP: s.client, Cache: &githubapp.Cache{}}
	logs := captureLogs(t)
	hits, failed := s.qqSearchIssuesFor(t.Context(), []string{"o/r"}, "synthetic-private-input", "", false)
	if !failed || len(hits) != 0 || !strings.Contains(logs.String(), "issue search failed") {
		t.Fatalf("没有记录搜索失败：hits=%v failed=%v logs=%s", hits, failed, logs.String())
	}
	for _, private := range []string{"synthetic-private-input", "/search/issues?", s.config.Chat.URL} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("QQ issue 搜索日志泄露了 %q：%s", private, logs.String())
		}
	}
}

func TestNiuTransTransportFailureDoesNotLogSignedURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/v2/doc/translate/upload", appID: "synthetic-private-app", apiKey: "synthetic-key"}
	logs := captureLogs(t)
	w := call(s, http.MethodGet, "/v1/niutrans/documents/synthetic-private-file", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, private := range []string{"authStr=", "synthetic-private-app", "synthetic-private-file"} {
		if strings.Contains(logs.String(), private) || strings.Contains(w.Body.String(), private) {
			t.Fatalf("上游错误泄露了 %q", private)
		}
	}
	if !strings.Contains(logs.String(), "upstream failed") || !strings.Contains(logs.String(), "/v1/niutrans/documents/{file_no}") {
		t.Fatalf("没有记录失败原因和路由模式：%s", logs.String())
	}
}

func TestUpstreamTimeoutLogDoesNotIncludeURL(t *testing.T) {
	logs := captureLogs(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/cloud/candidates", nil)
	r.Pattern = "GET /v1/cloud/candidates"
	w := httptest.NewRecorder()
	err := fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: "https://example.test/?api_key=synthetic-private-key", Err: context.DeadlineExceeded})
	upstreamError(w, r, err)
	if w.Code != http.StatusGatewayTimeout || strings.Contains(logs.String(), "synthetic-private-key") || !strings.Contains(logs.String(), "upstream timed out") {
		t.Fatalf("status=%d logs=%s", w.Code, logs.String())
	}
}

func TestChatStreamFallbackDoesNotLogUpstreamURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input chatRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input.Model == "server-model" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, contentFrame("备用")+"data: [DONE]\n\n")
	})
	s.config.Chat.URL += "?api_key=synthetic-private-key"
	s.config.Chat.FallbackModel = "fallback-model"
	logs := captureLogs(t)
	w := call(s, http.MethodPost, "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"test"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "备用") || !strings.Contains(logs.String(), "trying the fallback model") {
		t.Fatalf("status=%d body=%s logs=%s", w.Code, w.Body.String(), logs.String())
	}
	if strings.Contains(logs.String(), "synthetic-private-key") {
		t.Fatal("备用模型日志泄露了上游查询串")
	}
}

func TestQQFallbackDoesNotLogUpstreamURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var input chatRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input.Model == "server-model" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		respond(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"answer":"合成备用回答"}`}, "finish_reason": "stop"}}})
	})
	s.config.Chat.URL += "?api_key=synthetic-private-key&query=synthetic-private-input"
	s.qq = &qqBot{config: QQBotConfig{Model: "server-model", FallbackModel: "fallback-model"}}
	logs := captureLogs(t)
	var out struct {
		Answer string `json:"answer"`
	}
	if err := s.qqChatModel(t.Context(), "server-model", "合成系统说明", "合成用户输入", &out); err != nil || out.Answer != "合成备用回答" {
		t.Fatal(out, err)
	}
	if !strings.Contains(logs.String(), "trying the fallback model") {
		t.Fatal("未记录主模型失败", logs.String())
	}
	for _, private := range []string{"synthetic-private-key", "synthetic-private-input", "api_key="} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("QQ 回退日志泄露了 %q", private)
		}
	}
}

func TestQQWebSearchDoesNotLogUpstreamURL(t *testing.T) {
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	s.config.Chat.URL += "/chat/completions?api_key=synthetic-private-key"
	s.qq = &qqBot{config: QQBotConfig{Model: "server-model"}}
	logs := captureLogs(t)
	if result := s.qqWebSearch(t.Context(), "合成查询"); result != "联网搜索失败。" {
		t.Fatal(result)
	}
	if !strings.Contains(logs.String(), "web search failed") || strings.Contains(logs.String(), "synthetic-private-key") {
		t.Fatal("联网搜索错误日志未脱敏", logs.String())
	}
}

// 共享上游错误的文本应能安全记录，同时保留超时、取消及原始传输类型供调用方判断。
func TestJSONUpstreamErrorsKeepClassificationWithoutURL(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancelled"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("已经结束的请求仍发往上游") })
			ctx, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			if timeout {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.Chat.URL+"?api_key=synthetic-private-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.doUpstream(req)
			if !errors.Is(err, want) {
				t.Fatal("丢失了取消或超时分类", err)
			}
			var transport *url.Error
			if !errors.As(err, &transport) {
				t.Fatal("丢失了传输错误类型", err)
			}
			if strings.Contains(err.Error(), "synthetic-private-key") || strings.Contains(err.Error(), "api_key=") {
				t.Error("共享上游错误文本泄露了请求地址")
			}
		})
	}
}
