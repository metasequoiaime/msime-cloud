package server

import (
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/diagnostics"
)

// diagnosticsMCP 返回 /mcp/s/{id} 的处理器；没有用户数据库时快照无处可读，返回 503。
func (s *Server) diagnosticsMCP() http.Handler {
	if s.accounts == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, 503, "user_auth_disabled") })
	}
	return diagnostics.Handler(s.accounts)
}
