package server

import (
	"errors"
	"net/http"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

// lookupAdminQQDraft 在调用方检查数据库可用后，共用草稿的只读查询及错误响应。
func (s *Server) lookupAdminQQDraft(w http.ResponseWriter, r *http.Request, id int64) (account.QQDraft, bool) {
	draft, err := s.accounts.QQDraft(r.Context(), id)
	if errors.Is(err, account.ErrQQDraftNotFound) {
		fail(w, 404, "not_found")
		return account.QQDraft{}, false
	}
	if err != nil {
		fail(w, 503, "database_unavailable")
		return account.QQDraft{}, false
	}
	return draft, true
}
