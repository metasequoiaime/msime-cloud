package publictools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools 共用注册与结果包装，目录和调用入口由各自的服务显式提供。
func registerTools(server *mcp.Server, execute Execute, definitions []*mcp.Tool, call func(context.Context, Execute, string, json.RawMessage) (Result, error)) {
	for _, tool := range definitions {
		server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := call(ctx, execute, tool.Name, request.Params.Arguments)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
			}
			data, _ := json.Marshal(result)
			return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
		})
	}
}
