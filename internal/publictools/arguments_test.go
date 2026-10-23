package publictools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestToolArgumentValidationAcrossCatalogs(t *testing.T) {
	for _, entry := range []struct {
		label, name, foreign, valid, unknownField string
		catalog                                   map[string]*jsonschema.Resolved
		call                                      func(context.Context, Execute, string, json.RawMessage) (Result, error)
	}{
		{"公共", "query_public_data", "get_qq_draft", `{"resource":"plugins"}`, `{"resource":"plugins","scope":"mine"}`, schemas(), Call},
		{"管理员", "get_qq_draft", "query_public_data", `{"id":"D12"}`, `{"id":"D12","scope":"mine"}`, adminSchemas(), AdminCall},
	} {
		t.Run(entry.label, func(t *testing.T) {
			for _, tc := range []struct {
				label, name, arguments, want string
			}{
				{"合法参数", entry.name, entry.valid, ""},
				{"恰好达到字节上限", entry.name, entry.valid + strings.Repeat(" ", MaxArguments-len(entry.valid)), ""},
				{"超过字节上限", entry.name, entry.valid + strings.Repeat(" ", MaxArguments+1-len(entry.valid)), "invalid_tool_arguments"},
				{"空参数缺少必填字段", entry.name, "", "invalid_tool_arguments"},
				{"空对象缺少必填字段", entry.name, "{}", "invalid_tool_arguments"},
				{"空白不是对象", entry.name, " \t", "invalid_tool_arguments"},
				{"拒绝空值", entry.name, "null", "invalid_tool_arguments"},
				{"拒绝数组", entry.name, "[]", "invalid_tool_arguments"},
				{"拒绝截断的对象", entry.name, "{", "invalid_tool_arguments"},
				{"拒绝额外的对象", entry.name, entry.valid + " {}", "invalid_tool_arguments"},
				{"拒绝未知字段", entry.name, entry.unknownField, "invalid_tool_arguments"},
				{"拒绝另一目录的工具", entry.foreign, entry.valid, "unknown_tool"},
				{"未知工具先于参数解码", "synthetic_unknown", "{", "unknown_tool"},
				{"未知工具先于长度检查", "synthetic_unknown", strings.Repeat(" ", MaxArguments+1), "unknown_tool"},
			} {
				t.Run(tc.label, func(t *testing.T) {
					arguments := json.RawMessage(tc.arguments)
					if err := validateToolArguments(entry.catalog, tc.name, arguments); argumentErrorCode(err) != tc.want {
						t.Fatalf("共用校验错误=%v，期望=%q", err, tc.want)
					}
					executed := false
					_, err := entry.call(t.Context(), func(context.Context, Request) (json.RawMessage, error) {
						executed = true
						return json.RawMessage(`{"synthetic":true}`), nil
					}, tc.name, arguments)
					if argumentErrorCode(err) != tc.want || executed != (tc.want == "") {
						t.Fatalf("入口校验错误=%v，执行=%t，期望错误=%q", err, executed, tc.want)
					}
				})
			}
		})
	}
}

func TestAdminListArgumentsKeepEmptyInputDefaults(t *testing.T) {
	for _, name := range []string{"list_qq_drafts", "list_qq_doc_gaps"} {
		for _, arguments := range []json.RawMessage{nil, {}, json.RawMessage(`{}`)} {
			if err := validateToolArguments(adminSchemas(), name, arguments); err != nil {
				t.Fatal(err)
			}
			var request Request
			_, err := AdminCall(t.Context(), func(_ context.Context, r Request) (json.RawMessage, error) {
				request = r
				return json.RawMessage(`{"items":[]}`), nil
			}, name, arguments)
			path := "/api/qq-bot/drafts?limit=20&offset=0"
			if name == "list_qq_doc_gaps" {
				path = "/api/qq-bot/doc-gaps?limit=20&offset=0"
			}
			if err != nil || request.Path != path || request.Method != "GET" || !request.NeedsToken {
				t.Fatalf("空参数默认请求改变：%+v，%v", request, err)
			}
		}
	}
}

func argumentErrorCode(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
