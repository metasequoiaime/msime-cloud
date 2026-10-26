package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
	"github.com/metasequoiaime/MSIME-Backend/internal/publictools"
)

// qqPublicToolCapabilities 让回答模型和现查模型使用同一个能力目录，避免没触发现查时否认已接入的工具。
func qqPublicToolCapabilities() string {
	var text strings.Builder
	text.WriteString("你在群里已经接入这些内置的只读公共查询工具，用户直接提问即可，无需安装或连接 MCP：\n")
	for _, tool := range publictools.Definitions() {
		fmt.Fprintf(&text, "- %s：%s\n", tool.Name, tool.Description)
	}
	text.WriteString("这些工具与 msime-cloud 的 MCP、CLI 共用能力。用户问能不能用 MCP 或 CLI 时，先说明可以在群里直接查词库和公共数据，不要笼统说「群聊不能调用 MCP」而省略已接入的查询能力；需要区分时再说明：外部 MCP 客户端通过 msime-cloud mcp（stdio）连接，机器人自身不连接任意外部 MCP 服务器，也不执行任意 shell 或 CLI 命令。只有实际调用成功才说已查到；工具失败时按返回结果说明暂时不可用，不把失败说成没有词条。")
	return text.String()
}

// qqPublicTool 在已鉴权、限流的 QQ 消息内复用现有处理器。工具层只能构造公共路径，
// 请求不带账号或管理令牌；社区处理器仍按匿名访问规则检查可见性并使用共享数据库限流。
func (s *Server) qqPublicTool(ctx context.Context, name string, arguments json.RawMessage) string {
	result, err := publictools.Call(ctx, func(ctx context.Context, query publictools.Request) (json.RawMessage, error) {
		request, err := httpclient.NewJSONBodyRequest(ctx, query.Method, query.Path, query.Body)
		if err != nil {
			return nil, errors.New("invalid_tool_request")
		}
		// 固定内部身份使公共社区请求共享同一个按地址限流桶，不使用群友的真实资料。
		request.RemoteAddr = "127.0.0.1:0"
		response := &qqPublicResponse{header: make(http.Header)}
		s.mux.ServeHTTP(response, request)
		if response.status < 200 || response.status >= 300 {
			return nil, fmt.Errorf("public_api_status_%d", response.status)
		}
		if response.tooLarge {
			return nil, errors.New("public_api_response_too_large")
		}
		return json.RawMessage(response.body.Bytes()), nil
	}, name, arguments)
	if err != nil {
		return "水杉公共数据查询失败：" + err.Error() + "。不能据此判断没有词条或作品。"
	}
	data, _ := json.Marshal(result)
	return "据水杉公共数据：" + qqTruncate(string(data), qqResearchResultRunes)
}

type qqPublicResponse struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	tooLarge bool
}

func (r *qqPublicResponse) Header() http.Header { return r.header }
func (r *qqPublicResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *qqPublicResponse) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if len(data) > publictools.MaxResponse-r.body.Len() {
		r.tooLarge = true
		return 0, io.ErrShortWrite
	}
	return r.body.Write(data)
}
