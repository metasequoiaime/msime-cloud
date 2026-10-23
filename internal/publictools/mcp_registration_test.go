package publictools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registrationClient(t *testing.T, execute Execute) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "synthetic", Version: "test"}, nil)
	registerTools(server, execute, Definitions(), Call)
	a, b := mcp.NewInMemoryTransports()
	session, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestRegisterToolsPreservesEachDispatchAndBothResultFormats(t *testing.T) {
	client := registrationClient(t, func(_ context.Context, r Request) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"method": r.Method, "path": r.Path})
	})
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil || len(listed.Tools) != len(Definitions()) {
		t.Fatalf("工具目录变化：%v，%v", listed, err)
	}
	for _, tc := range []struct {
		name, method, path string
		args               map[string]any
	}{
		{"lookup_dictionary", "POST", "/v1/input/candidates", map[string]any{"text": "hecheng"}},
		{"browse_input_catalog", "GET", "/v1/catalog/emoji?category=&limit=20&offset=0&q=", map[string]any{"kind": "emoji"}},
		{"query_public_data", "GET", "/v1/community/plugins", map[string]any{"resource": "plugins"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil || result.IsError || len(result.Content) != 1 {
				t.Fatalf("工具调用失败：%v，%v", result, err)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("结果不再包含文本：%T", result.Content[0])
			}
			data, err := json.Marshal(map[string]string{"method": tc.method, "path": tc.path})
			if err != nil {
				t.Fatal(err)
			}
			want := Result{Source: tc.path, Data: data}
			structured, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range []string{text.Text, string(structured)} {
				var got Result
				if err := json.Unmarshal([]byte(raw), &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("工具结果与其调用目标不匹配：%s，期望 %+v，%v", raw, want, err)
				}
			}
		})
	}
}

func TestRegisterToolsKeepsCallFailuresAsToolErrors(t *testing.T) {
	client := registrationClient(t, func(context.Context, Request) (json.RawMessage, error) {
		return nil, errors.New("synthetic_failure")
	})
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"resource": "plugins"}, "synthetic_failure"},
		{map[string]any{"resource": "plugins", "scope": "mine"}, "invalid_tool_arguments"},
	} {
		result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "query_public_data", Arguments: tc.args})
		if err != nil || !result.IsError || result.StructuredContent != nil || len(result.Content) != 1 {
			t.Fatalf("调用错误没有保留工具错误格式：%v，%v", result, err)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || text.Text != tc.want {
			t.Fatalf("工具错误文案变化：%v，期望 %q", result.Content, tc.want)
		}
	}
}
