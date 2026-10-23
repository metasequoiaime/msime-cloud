package publictools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPublicToolRequests(t *testing.T) {
	for _, tc := range []struct {
		name, args, method, path string
		token                    bool
	}{
		{"lookup_dictionary", `{"text":"hecheng"}`, "POST", "/v1/input/candidates", true},
		{"lookup_dictionary", `{"text":"合成词"}`, "POST", "/v1/input/listed", true},
		{"lookup_dictionary", `{"operation":"candidates","text":"合成词","scheme":"pinyin","profile":"xiaohe"}`, "POST", "/v1/input/listed", true},
		{"lookup_dictionary", `{"operation":"listed","text":"合成词"}`, "POST", "/v1/input/listed", true},
		{"lookup_dictionary", `{"operation":"listed","text":"合成词","scheme":"pinyin","profile":"xiaohe"}`, "POST", "/v1/input/listed", true},
		{"lookup_dictionary", `{"text":"hecheng","scheme":"shuangpin"}`, "POST", "/v1/input/candidates", true},
		{"lookup_dictionary", `{"text":"合成词","scheme":"wubi"}`, "POST", "/v1/input/candidates", true},
		{"lookup_dictionary", `{"operation":"annotate","text":"合成"}`, "POST", "/v1/input/annotate", true},
		{"lookup_dictionary", `{"text":"合成词","scheme":"shuangpin","profile":"ziranma"}`, "POST", "/v1/input/annotate", true},
		{"lookup_dictionary", `{"operation":"gloss","text":"synthetic","direction":"en-zh"}`, "POST", "/v1/input/gloss", true},
		{"browse_input_catalog", `{"kind":"symbols","q":"a&scope=mine"}`, "GET", "/v1/catalog/symbols?category=&limit=20&offset=0&q=a%26scope%3Dmine", true},
		{"query_public_data", `{"resource":"notices","platform":"android","channel":"stable"}`, "GET", "/v1/notices?channel=stable&platform=android", false},
		{"query_public_data", `{"resource":"community_stats"}`, "GET", "/v1/community/stats", false},
		{"query_public_data", `{"resource":"resources","kind":"dictionary","q":"合成","offset":20}`, "GET", "/v1/community/resources?kind=dictionary&offset=20&q=%E5%90%88%E6%88%90", false},
		{"query_public_data", `{"resource":"plugins","id":"synthetic-pack"}`, "GET", "/v1/community/plugins/synthetic-pack", false},
		{"query_public_data", `{"resource":"skins"}`, "GET", "/v1/community/skins", false},
		{"query_public_data", `{"resource":"candidate_skins","id":"synthetic-skin"}`, "GET", "/v1/community/candidate-skins/synthetic-skin", false},
		{"query_public_data", `{"resource":"input_capabilities"}`, "GET", "/v1/input/capabilities", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			executed := false
			result, err := Call(t.Context(), func(ctx context.Context, r Request) (json.RawMessage, error) {
				executed = true
				if r.Method != tc.method || r.Path != tc.path || r.NeedsToken != tc.token {
					t.Fatalf("错误请求：%+v", r)
				}
				if r.Method == "POST" && (!json.Valid(r.Body) || strings.Contains(string(r.Body), `"operation"`)) {
					t.Fatalf("错误请求体：%s", r.Body)
				}
				if r.Path == "/v1/input/listed" && (strings.Contains(string(r.Body), `"scheme"`) || strings.Contains(string(r.Body), `"profile"`)) {
					t.Fatalf("词语检查不能带候选方案：%s", r.Body)
				}
				return json.RawMessage(`{"items":["synthetic"]}`), nil
			}, tc.name, json.RawMessage(tc.args))
			if err != nil || !executed || result.Source != tc.path || !strings.Contains(string(result.Data), "synthetic") {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestHanziShuangpinQueryPreservesEncodingOptions(t *testing.T) {
	for _, operation := range []string{"candidates", "annotate"} {
		args, _ := json.Marshal(map[string]any{"operation": operation, "text": "合成", "scheme": "shuangpin", "profile": "microsoft", "schema": "xiaohe", "direction": "en-zh", "time": "2026-01-02T03:04:05Z", "timezone": "UTC"})
		_, err := Call(t.Context(), func(_ context.Context, request Request) (json.RawMessage, error) {
			var body map[string]any
			if json.Unmarshal(request.Body, &body) != nil || request.Path != "/v1/input/annotate" || body["text"] != "合成" || body["scheme"] != "shuangpin" || body["profile"] != "microsoft" || len(body) != 3 {
				t.Fatalf("双拼汉字反查未保留所属选项：%+v", request)
			}
			return json.RawMessage(`{"code":"synthetic"}`), nil
		}, "lookup_dictionary", args)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnsafeArgumentsNeverExecute(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"shell", `{"command":"ls"}`},
		{"lookup_dictionary", `{}`},
		{"lookup_dictionary", `null`},
		{"lookup_dictionary", `[]`},
		{"lookup_dictionary", `{"text":"a"} {}`},
		{"lookup_dictionary", `{"text":"a","operation":"../users/me"}`},
		{"lookup_dictionary", `{"text":"a","scheme":"other"}`},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","profile":"microsoft"}`},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","profile":"shoudao"}`},
		{"lookup_dictionary", `{"operation":"helpcode","text":"木","schema":"../private","profile":"xiaohe"}`},
		{"lookup_dictionary", `{"text":"a","limit":0}`},
		{"lookup_dictionary", `{"text":"a","limit":51}`},
		{"lookup_dictionary", `{"text":"a","resources":"/private"}`},
		{"lookup_dictionary", `{"text":"a","snapshot":"private"}`},
		{"lookup_dictionary", `{"text":"` + strings.Repeat("x", MaxArguments) + `"}`},
		{"lookup_dictionary", `{"text":"` + strings.Repeat("x", 8193) + `"}`},
		{"browse_input_catalog", `{"kind":"../../private"}`},
		{"browse_input_catalog", `{"kind":"symbols","offset":-1}`},
		{"browse_input_catalog", `{"kind":"symbols","q":"` + strings.Repeat("x", 1025) + `"}`},
		{"query_public_data", `{"resource":"users/me"}`},
		{"query_public_data", `{"resource":"plugins","scope":"mine"}`},
		{"query_public_data", `{"resource":"candidate_skins","fields":"sync"}`},
		{"query_public_data", `{"resource":"resources"}`},
		{"query_public_data", `{"resource":"notices","id":"private"}`},
		{"query_public_data", `{"resource":"plugins","id":"../private"}`},
		{"query_public_data", `{"resource":"plugins","id":"a/download"}`},
		{"query_public_data", `{"resource":"plugins","id":"a%2fdownload"}`},
		{"query_public_data", `{"resource":"plugins","id":"a?q=x"}`},
		{"query_public_data", `{"resource":"plugins","kind":"private"}`},
		{"query_public_data", `{"resource":"plugins","platform":42}`},
		{"query_public_data", `{"resource":"community_stats","offset":-1}`},
	} {
		t.Run(tc.name+tc.args[:min(70, len(tc.args))], func(t *testing.T) {
			_, err := Call(t.Context(), func(context.Context, Request) (json.RawMessage, error) {
				t.Fatal("非法参数被执行")
				return nil, nil
			}, tc.name, json.RawMessage(tc.args))
			if err == nil {
				t.Fatal("没有拒绝参数")
			}
		})
	}
}

// 所有公共资源均可能收到其他资源的默认值，列表筛选和详情 ID 仍须保留其原有语义。
func TestPublicToolResourceRelevantOptions(t *testing.T) {
	for _, tc := range []struct {
		resource, id, path string
		token              bool
	}{
		{"notices", "", "/v1/notices?channel=stable&platform=android", false},
		{"community_stats", "", "/v1/community/stats", false},
		{"input_capabilities", "", "/v1/input/capabilities", true},
		{"resources", "", "/v1/community/resources?kind=dictionary&offset=20&q=synthetic", false},
		{"plugins", "", "/v1/community/plugins?offset=20&q=synthetic", false},
		{"skins", "", "/v1/community/skins?offset=20&q=synthetic", false},
		{"candidate_skins", "", "/v1/community/candidate-skins?offset=20&q=synthetic", false},
		{"resources", "synthetic-item", "/v1/community/resources/synthetic-item", false},
		{"plugins", "synthetic-item", "/v1/community/plugins/synthetic-item", false},
		{"skins", "synthetic-item", "/v1/community/skins/synthetic-item", false},
		{"candidate_skins", "synthetic-item", "/v1/community/candidate-skins/synthetic-item", false},
	} {
		t.Run(tc.resource+"/"+tc.id, func(t *testing.T) {
			args, err := json.Marshal(map[string]any{"resource": tc.resource, "id": tc.id, "q": "synthetic", "kind": "dictionary", "offset": 20, "platform": "android", "channel": "stable"})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			result, err := Call(t.Context(), func(_ context.Context, request Request) (json.RawMessage, error) {
				calls++
				if request.Method != http.MethodGet || request.Path != tc.path || request.NeedsToken != tc.token || len(request.Body) != 0 {
					t.Fatalf("公共请求参数错误：%+v", request)
				}
				return json.RawMessage(`{"synthetic":true}`), nil
			}, "query_public_data", args)
			if err != nil || calls != 1 || result.Source != tc.path {
				t.Fatalf("公共目录查询失败：calls=%d result=%+v err=%v", calls, result, err)
			}
		})
	}
}

func TestResponseAndCancellationBoundaries(t *testing.T) {
	for _, data := range []string{"not json", `{"text":"` + strings.Repeat("x", MaxResponse) + `"}`} {
		_, err := Call(t.Context(), func(context.Context, Request) (json.RawMessage, error) { return json.RawMessage(data), nil }, "query_public_data", json.RawMessage(`{"resource":"notices"}`))
		if err == nil {
			t.Fatal("非法响应被接受")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Call(ctx, func(ctx context.Context, _ Request) (json.RawMessage, error) { return nil, ctx.Err() }, "query_public_data", json.RawMessage(`{"resource":"notices"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHTTPAuthIsolationAndResponseLimits(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if len(r.Cookies()) > 0 {
			t.Error("公共工具收到会话 Cookie")
		}
		switch r.URL.Path {
		case "/v1/community/plugins":
			if r.Header.Get("Authorization") != "" {
				t.Error("公共目录收到令牌")
			}
		case "/v1/input/candidates":
			if r.Header.Get("Authorization") != "Bearer synthetic-token" {
				t.Error("词库没有显式令牌")
			}
			var data map[string]any
			_ = json.NewDecoder(r.Body).Decode(&data)
			if data["text"] != "hecheng" || r.Header.Get("Content-Type") != "application/json" {
				t.Error(data)
			}
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", MaxResponse+1))
			return
		case "/redirect":
			http.Redirect(w, r, "/private", http.StatusFound)
			return
		case "/private":
			t.Error("跟随了重定向")
			return
		case "/error":
			http.Error(w, "upstream-private-marker", 500)
			return
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	request, _ := http.NewRequest("GET", srv.URL, nil)
	jar.SetCookies(request.URL, []*http.Cookie{{Name: "private-session", Value: "synthetic-session"}})
	client := *srv.Client()
	client.Jar = jar
	execute, err := HTTP(&client, srv.URL, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, args string }{
		{"query_public_data", `{"resource":"plugins"}`},
		{"lookup_dictionary", `{"text":"hecheng"}`},
	} {
		if _, err := Call(t.Context(), execute, tc.name, json.RawMessage(tc.args)); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/large", "/redirect", "/error"} {
		_, err := execute(t.Context(), Request{Method: "GET", Path: path})
		if err == nil || strings.Contains(err.Error(), "private-marker") {
			t.Fatal(path, err)
		}
	}
	if requests != 5 {
		t.Fatal(requests)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = execute(ctx, Request{Method: "GET", Path: "/unavailable"}); err == nil {
		t.Fatal("取消后仍然成功")
	}
	for _, base := range []string{"bad", "file:///private", "https://example.test/?q=x", "https://user:secret@example.test", "https://example.test/#x"} {
		if _, err := HTTP(http.DefaultClient, base, ""); err == nil {
			t.Fatal(base)
		}
	}
}

func TestHTTPRejectsNilClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func() (Execute, error)
	}{
		{"public", func() (Execute, error) { return HTTP(nil, "https://example.test", "") }},
		{"admin", func() (Execute, error) { return AdminHTTP(nil, "https://example.test", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.make(); err == nil {
				t.Fatal("nil HTTP client should be rejected")
			}
		})
	}
}

func TestMCPToolCatalogAndCalls(t *testing.T) {
	execute := func(_ context.Context, r Request) (json.RawMessage, error) {
		return json.RawMessage(`{"query":"synthetic"}`), nil
	}
	server := NewServer(execute, "test")
	a, b := mcp.NewInMemoryTransports()
	session, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 3 {
		t.Fatal(tools, err)
	}
	for _, def := range tools.Tools {
		if !Has(def.Name) || def.Annotations == nil || !def.Annotations.ReadOnlyHint {
			t.Fatal(def)
		}
	}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "lookup_dictionary", Arguments: map[string]any{"text": "hecheng"}})
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "synthetic") {
		t.Fatal(result, err)
	}
	result, err = client.CallTool(t.Context(), &mcp.CallToolParams{Name: "query_public_data", Arguments: map[string]any{"resource": "plugins", "scope": "mine"}})
	if err != nil || !result.IsError {
		t.Fatal(result, err)
	}
	if len(ChatDefinitions()) != len(Definitions()) || Has("private_tool") {
		t.Fatal("工具目录不一致")
	}
}
