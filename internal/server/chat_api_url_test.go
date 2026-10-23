package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestChatAPIURL(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"标准地址", "https://upstream.example.test/v1/chat/completions", "https://upstream.example.test/v1/API"},
		{"自定义前缀", "https://upstream.example.test/proxy/v2/chat/completions", "https://upstream.example.test/proxy/v2/API"},
		{"根地址", "https://upstream.example.test", "https://upstream.example.test/API"},
		{"目录地址", "https://upstream.example.test/v1/", "https://upstream.example.test/v1/API"},
		{"其他路径追加", "https://upstream.example.test/custom", "https://upstream.example.test/custom/API"},
		{"尾斜杠保持追加", "https://upstream.example.test/v1/chat/completions/", "https://upstream.example.test/v1/chat/completions/API"},
		{"清理路径", "https://upstream.example.test/tenant//a/../chat/completions", "https://upstream.example.test/tenant/API"},
		{"保留查询", "https://upstream.example.test/v1/chat/completions?x=%2f&x=2", "https://upstream.example.test/v1/API?x=%2f&x=2"},
		{"保留空查询", "https://upstream.example.test/v1/chat/completions?", "https://upstream.example.test/v1/API?"},
		{"保留片段", "https://upstream.example.test/v1/chat/completions#section", "https://upstream.example.test/v1/API#section"},
		{"空片段不留标记", "https://upstream.example.test/v1/chat/completions#", "https://upstream.example.test/v1/API"},
		{"转义路径沿用解码结果", "https://upstream.example.test/tenant%2Fid/chat/completions", "https://upstream.example.test/tenant/id/API"},
		{"中文路径", "https://upstream.example.test/水/chat/completions", "https://upstream.example.test/%E6%B0%B4/API"},
		{"保留端口", "https://upstream.example.test:8443/v1/chat/completions", "https://upstream.example.test:8443/v1/API"},
		{"保留用户信息", "https://user:synthetic@upstream.example.test/v1/chat/completions", "https://user:synthetic@upstream.example.test/v1/API"},
		{"保留HTTP", "http://upstream.example.test/v1/chat/completions", "http://upstream.example.test/v1/API"},
		{"保留相对协议", "//upstream.example.test/v1/chat/completions", "//upstream.example.test/v1/API"},
		{"沿用Host判断", "https://:443/v1/chat/completions", "https://:443/v1/API"},
		{"空地址", "", ""},
		{"相对路径", "/v1/chat/completions", ""},
		{"无主机", "https:///v1/chat/completions", ""},
		{"非法转义", "https://upstream.example.test/%zz", ""},
		{"非法端口", "https://upstream.example.test:bad/v1/chat/completions", ""},
		{"非法控制字符", "https://upstream.example.test/\n", ""},
	} {
		for _, api := range []string{"embeddings", "messages"} {
			t.Run(tc.name+"/"+api, func(t *testing.T) {
				want := strings.ReplaceAll(tc.want, "API", api)
				if got := chatAPIURL(tc.raw, api); got != want {
					t.Fatalf("派生地址为 %q，期望 %q", got, want)
				}
			})
		}
	}
}

func TestChatAPIConsumersPreserveRequests(t *testing.T) {
	for _, api := range []string{"embeddings", "messages"} {
		t.Run(api, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/proxy/v1/"+api+"?tenant=synthetic" {
					t.Errorf("上游请求目标改变：%s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
					t.Error("上游认证或内容协商请求头改变")
				}
				var body struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				if api == "embeddings" {
					if body.Model != "synthetic-embedding" {
						t.Errorf("向量模型改变：%q", body.Model)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": make([]float32, account.QQEmbeddingDims)}}})
				} else {
					if body.Model != "synthetic-chat" || r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("联网搜索模型或协议版本改变")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text", "text": "合成搜索结果"}}})
				}
			}))
			defer upstream.Close()
			s := &Server{
				client: upstream.Client(),
				config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: upstream.URL + "/proxy/v1/chat/completions?tenant=synthetic", token: "synthetic-token"}}},
				qq:     &qqBot{config: QQBotConfig{Model: "synthetic-chat", EmbeddingModel: "synthetic-embedding"}},
			}
			if api == "embeddings" {
				vectors, err := s.qqEmbed(t.Context(), []string{"合成文本"})
				if err != nil || len(vectors) != 1 || len(vectors[0]) != account.QQEmbeddingDims {
					t.Fatalf("向量请求失败：%v", err)
				}
			} else if got := s.qqWebSearch(t.Context(), " 合成问题 "); got != "合成搜索结果" {
				t.Fatalf("联网搜索失败：%q", got)
			}
			if calls.Load() != 1 {
				t.Fatal("上游请求次数改变")
			}
		})
	}
}

func TestQQEmbeddingURLFeatureGates(t *testing.T) {
	for _, tc := range []struct {
		name string
		bot  *qqBot
		want string
	}{
		{"未启用答疑", nil, ""},
		{"停用向量", &qqBot{config: QQBotConfig{EmbeddingModel: "none"}}, ""},
		{"启用向量", &qqBot{config: QQBotConfig{EmbeddingModel: "synthetic-model"}}, "https://upstream.example.test/v1/embeddings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{qq: tc.bot, config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://upstream.example.test/v1/chat/completions"}}}}
			if got := s.qqEmbeddingURL(); got != tc.want {
				t.Fatalf("向量地址为 %q，期望 %q", got, tc.want)
			}
		})
	}
}
