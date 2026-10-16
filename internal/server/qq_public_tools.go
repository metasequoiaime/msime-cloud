package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/publictools"
)

// qqPublicTool 在已鉴权、限流的 QQ 消息内复用现有处理器。工具层只能构造公共路径，
// 请求不带账号或管理令牌；社区处理器仍按匿名访问规则检查可见性并使用共享数据库限流。
func (s *Server) qqPublicTool(ctx context.Context, name string, arguments json.RawMessage) string {
	result, err := publictools.Call(ctx, func(ctx context.Context, query publictools.Request) (json.RawMessage, error) {
		request, err := http.NewRequestWithContext(ctx, query.Method, query.Path, bytes.NewReader(query.Body))
		if err != nil {
			return nil, errors.New("invalid_tool_request")
		}
		request.Header.Set("Content-Type", "application/json")
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
