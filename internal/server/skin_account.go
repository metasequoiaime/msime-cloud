package server

import (
	"net/http"
	"strings"
)

// ensureUserOwnerActive 在会产生上游副作用的请求开始前锁住用户行并复查封禁状态。
// 配置的客户端令牌没有对应账号，不需要这一步。
func (s *Server) ensureUserOwnerActive(w http.ResponseWriter, r *http.Request) bool {
	owner, _ := r.Context().Value(skinJobOwnerKey{}).(string)
	user, ok := strings.CutPrefix(owner, "user:")
	if !ok || user == "" {
		return true
	}
	err := s.accounts.EnsureUserActive(r.Context(), user)
	if writeSkinAccountError(w, err) {
		return false
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "auth_unavailable")
		return false
	}
	return true
}
